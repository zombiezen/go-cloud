// Copyright 2019 The Go Cloud Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"flag"
	"go/ast"
	"go/format"
	"go/printer"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/exp/errors"
	"golang.org/x/exp/errors/fmt"
	"golang.org/x/tools/go/packages"
)

func add(ctx context.Context, pctx *processContext, args []string) error {
	f := flag.NewFlagSet("gocloud add", flag.ContinueOnError)
	f.SetOutput(pctx.stderr)
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		f.SetOutput(pctx.stdout)
		f.PrintDefaults()
		return nil
	} else if err != nil {
		return usagef("%v", err)
	}
	if f.NArg() != 1 {
		return usagef("must specify one argument")
	}
	if f.Arg(0) != "blob" {
		return fmt.Errorf("unknown service %q. prototype only knows blob")
	}
	cfg := &packages.Config{
		Context:    ctx,
		Mode:       packages.LoadSyntax,
		Dir:        pctx.dir,
		Env:        pctx.env,
		BuildFlags: []string{"-tags=wireinject"},
	}
	// TODO(light): Is this pattern portable across build systems?
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return fmt.Errorf("add: %w", err)
	}
	pkg := findMainPackage(pkgs)
	if pkg == nil {
		return errors.New("no suitable Go package found")
	}
	if _, obj := pkg.Types.Scope().LookupParent("provideBlob", 0); obj != nil {
		return errors.New("provideBlob function already exists; canceling")
	}

	rset := make(rewriteSet)
	injectors := allInjectors(pkg.TypesInfo, pkg.Syntax)
	var modFile *rewrittenFile
	addedToWire := false
	// TODO(light): Multiple injectors is okay if one is called "setup".
	if len(injectors) == 1 && fileForPos(pkg.Fset, pkg.Syntax, injectors[0].Pos()) != nil {
		modFile = rset.rewriteFile(pkg.Fset, fileForPos(pkg.Fset, pkg.Syntax, injectors[0].Pos()), pkg.Imports)
		injectors[0].Args = append(injectors[0].Args, &ast.Ident{Name: "provideBlob"})
		addedToWire = true
	} else {
		astFile, path := defaultSetupFile(pkg.Fset, pkg.Syntax)
		if astFile == nil {
			modFile = rset.newFile(path)
		} else {
			modFile = rset.rewriteFile(pkg.Fset, astFile, pkg.Imports)
		}
	}
	addedAppField := false
	if appStruct := findStructType(pkg.Syntax, "application"); appStruct != nil {
		if f := fileForPos(pkg.Fset, pkg.Syntax, appStruct.Pos()); f != nil {
			appFile := rset.rewriteFile(pkg.Fset, f, pkg.Imports)
			appStruct.Fields.List = append(appStruct.Fields.List, &ast.Field{
				Names: []*ast.Ident{{Name: "bucket"}},
				Type: &ast.StarExpr{X: &ast.SelectorExpr{
					X:   &ast.Ident{Name: appFile.pkg("blob", "gocloud.dev/blob")},
					Sel: &ast.Ident{Name: "Bucket"},
				}},
			})
			addedAppField = true
		}
	}
	fmt.Fprintf(&modFile.newCode, "\nfunc provideBlob() *%s.Bucket {\n", modFile.pkg("blob", "gocloud.dev/blob"))
	fmt.Fprintf(&modFile.newCode, "\treturn %s.OpenBucket(nil)\n", modFile.pkg("memblob", "gocloud.dev/blob/memblob"))
	fmt.Fprintf(&modFile.newCode, "}\n")

	for _, rf := range rset {
		if err := rf.flush(); err != nil {
			return fmt.Errorf("add: %w", err)
		}
	}
	switch {
	case addedToWire && addedAppField:
		fmt.Fprintf(pctx.stdout, "Added provideBlob to %s. Use the application.bucket field in your handler.\n", filepath.Base(modFile.path))
		// TODO(light): Run Wire automatically.
	case addedToWire:
		fmt.Fprintf(pctx.stdout, "Added provideBlob to %s and to Wire injector.\n", filepath.Base(modFile.path))
		fmt.Fprintf(pctx.stdout, "Could not find application struct, so you must\n")
		fmt.Fprintf(pctx.stdout, "manually inject the *blob.Bucket.\n")
	case addedAppField:
		fmt.Fprintf(pctx.stdout, "Added provideBlob to %s and to application struct.\n", filepath.Base(modFile.path))
		fmt.Fprintf(pctx.stdout, "Could not find your main Wire injector, so you\n")
		fmt.Fprintf(pctx.stdout, "must manually inject the *blob.Bucket to your\n")
		fmt.Fprintf(pctx.stdout, "application.\n")
	default:
	}
	return nil
}

type rewriteSet map[string]*rewrittenFile

func (set rewriteSet) rewriteFile(fset *token.FileSet, f *ast.File, imports map[string]*packages.Package) *rewrittenFile {
	path := fset.File(f.Package).Name()
	if rf := set[path]; rf != nil {
		return rf
	}
	importNames := make(map[string]string, len(imports))
	for k, v := range imports {
		importNames[k] = v.Name
	}
	rf := &rewrittenFile{
		path:        path,
		fset:        fset,
		original:    f,
		importNames: importNames,
	}
	set[path] = rf
	return rf
}

func (set rewriteSet) newFile(path string) *rewrittenFile {
	if rf := set[path]; rf != nil {
		return rf
	}
	rf := &rewrittenFile{
		path:       path,
		newImports: make(map[string]string),
	}
	set[path] = rf
	return rf
}

type rewrittenFile struct {
	path        string
	fset        *token.FileSet
	original    *ast.File
	importNames map[string]string
	newImports  map[string]string
	newCode     bytes.Buffer
}

func (rf *rewrittenFile) pkg(defaultName, path string) string {
	// Totally new file, so get or create in the newImports map.
	if rf.original == nil {
		if id := rf.newImports[path]; id != "" {
			return id
		}
		// TODO(light): This is not checking for name collisions at all.
		rf.newImports[path] = defaultName
		return defaultName
	}

	// See if this has already been imported.
	var spec *ast.ImportSpec
	ast.Inspect(rf.original, func(node ast.Node) bool {
		if spec != nil {
			return false
		}
		switch node := node.(type) {
		case *ast.File, *ast.GenDecl:
			return true
		case *ast.ImportSpec:
			if val, _ := strconv.Unquote(node.Path.Value); val == path {
				spec = node
			}
		}
		return false
	})
	if spec != nil {
		if spec.Name != nil {
			return spec.Name.Name
		}
		return rf.importNames[path]
	}

	// Add import to list.
	impDecl := getOrCreateImportDecl(rf.original)
	impDecl.Specs = append(impDecl.Specs, &ast.ImportSpec{
		Path: stringLit(path),
	})
	return defaultName
}

func (rf *rewrittenFile) flush() error {
	var unformatted *bytes.Buffer
	if rf.original == nil {
		unformatted = &rf.newCode
	} else {
		unformatted = new(bytes.Buffer)
		printer.Fprint(unformatted, rf.fset, rf.original)
		rf.newCode.WriteTo(unformatted)
	}
	formatted, err := format.Source(unformatted.Bytes())
	if err != nil {
		os.Stderr.Write(unformatted.Bytes())
		return fmt.Errorf("format generated code: %w", err)
	}
	// TODO(light): Move file over atomically to avoid clobbering code.
	if err := ioutil.WriteFile(rf.path, formatted, 0666); err != nil {
		return fmt.Errorf("update go file: %w", err)
	}
	return nil
}

func getOrCreateImportDecl(f *ast.File) *ast.GenDecl {
	if len(f.Decls) > 0 {
		if decl, ok := f.Decls[0].(*ast.GenDecl); ok && decl.Tok == token.IMPORT {
			return decl
		}
	}
	decl := &ast.GenDecl{
		Tok: token.IMPORT,
	}
	f.Decls = append([]ast.Decl{decl}, f.Decls...)
	return decl
}

func stringLit(val string) *ast.BasicLit {
	return &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(val)}
}

func fileForPos(fset *token.FileSet, files []*ast.File, pos token.Pos) *ast.File {
	tokFile := fset.File(pos)
	for _, astFile := range files {
		if fset.File(astFile.Package) == tokFile {
			return astFile
		}
	}
	return nil
}

func findFunc(files []*ast.File, name string) *ast.FuncDecl {
	for _, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
				return fn
			}
		}
	}
	return nil
}

func findStructType(files []*ast.File, name string) *ast.StructType {
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				if spec, ok := spec.(*ast.TypeSpec); ok && spec.Name.Name == name {
					st, _ := spec.Type.(*ast.StructType)
					return st
				}
			}
		}
	}
	return nil
}

func findMainPackage(pkgs []*packages.Package) *packages.Package {
	if len(pkgs) == 0 {
		return nil
	}
	if len(pkgs) == 1 {
		return pkgs[0]
	}
	for _, pkg := range pkgs {
		// TODO(light): This picks first to match, which might not be right
		// (what if test package comes first).
		if pkg.Name != "main" {
			continue
		}
		if findFunc(pkg.Syntax, "main") != nil {
			return pkg
		}
	}
	return nil
}

func allInjectors(info *types.Info, files []*ast.File) []*ast.CallExpr {
	var injectors []*ast.CallExpr
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			build, err := findInjectorBuild(info, fn)
			if err == nil && build != nil {
				injectors = append(injectors, build)
			}
		}
	}
	return injectors
}

func defaultSetupFile(fset *token.FileSet, files []*ast.File) (*ast.File, string) {
	const base = "setup.go"
	for _, f := range files {
		path := fset.File(f.Package).Name()
		if filepath.Base(path) == base {
			return f, path
		}
	}
	return nil, filepath.Join(filepath.Dir(fset.File(files[0].Package).Name()), base)
}

// findInjectorBuild returns the wire.Build call if fn is an injector template.
// It returns nil if the function is not an injector template.
func findInjectorBuild(info *types.Info, fn *ast.FuncDecl) (*ast.CallExpr, error) {
	if fn.Body == nil {
		return nil, nil
	}
	numStatements := 0
	invalid := false
	var wireBuildCall *ast.CallExpr
	for _, stmt := range fn.Body.List {
		switch stmt := stmt.(type) {
		case *ast.ExprStmt:
			numStatements++
			if numStatements > 1 {
				invalid = true
			}
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			if qualifiedIdentObject(info, call.Fun) == types.Universe.Lookup("panic") {
				if len(call.Args) != 1 {
					continue
				}
				call, ok = call.Args[0].(*ast.CallExpr)
				if !ok {
					continue
				}
			}
			buildObj := qualifiedIdentObject(info, call.Fun)
			if buildObj == nil || buildObj.Pkg() == nil || !isWireImport(buildObj.Pkg().Path()) || buildObj.Name() != "Build" {
				continue
			}
			wireBuildCall = call
		case *ast.EmptyStmt:
			// Do nothing.
		case *ast.ReturnStmt:
			// Allow the function to end in a return.
			if numStatements == 0 {
				return nil, nil
			}
		default:
			invalid = true
		}

	}
	if wireBuildCall == nil {
		return nil, nil
	}
	if invalid {
		return nil, errors.New("a call to wire.Build indicates that this function is an injector, but injectors must consist of only the wire.Build call and an optional return")
	}
	return wireBuildCall, nil
}

func isWireImport(path string) bool {
	// TODO(light): This is depending on details of the current loader.
	const vendorPart = "vendor/"
	if i := strings.LastIndex(path, vendorPart); i != -1 && (i == 0 || path[i-1] == '/') {
		path = path[i+len(vendorPart):]
	}
	return path == "github.com/google/wire" || path == "github.com/google/go-cloud/wire"
}

// qualifiedIdentObject finds the object for an identifier or a
// qualified identifier, or nil if the object could not be found.
func qualifiedIdentObject(info *types.Info, expr ast.Expr) types.Object {
	switch expr := expr.(type) {
	case *ast.Ident:
		return info.ObjectOf(expr)
	case *ast.SelectorExpr:
		pkgName, ok := expr.X.(*ast.Ident)
		if !ok {
			return nil
		}
		if _, ok := info.ObjectOf(pkgName).(*types.PkgName); !ok {
			return nil
		}
		return info.ObjectOf(expr.Sel)
	default:
		return nil
	}
}
