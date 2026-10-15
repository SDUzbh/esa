// Command doxygen-go adapts Go declarations and their comments to Doxygen pages.
// It parses source only; it never imports, builds or executes the documented code.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: doxygen-go FILE.go (run from the repository root)")
		os.Exit(1)
	}
	root, err := os.Getwd()
	if err == nil {
		err = render(os.Stdout, root, os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// render emits comment-only Doxygen input, preserving Go syntax instead of
// inventing C++ types, inheritance or call graphs for a language it cannot parse.
func render(out io.Writer, root, path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("documentation input is outside the repository")
	}
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return err
	}
	relative = filepath.ToSlash(relative)
	// Hex encoding is reversible and prevents punctuation/path collisions.
	page := fmt.Sprintf("go_%x", relative)
	var body strings.Builder
	fmt.Fprintf(&body, "@page %s %s\n\nPackage: `%s`. Source: `%s`.\n\n", page, relative, file.Name.Name, relative)
	if file.Doc != nil {
		body.WriteString(file.Doc.Text() + "\n")
	}
	body.WriteString("Declarations retain Go syntax. This page is an AST-derived reference, not a call graph.\n\n")
	section := func(name, doc, declaration string, line int) {
		fmt.Fprintf(&body, "@section %s_%x %s\n\n", page, name, name)
		if doc != "" {
			body.WriteString(doc + "\n")
		}
		fmt.Fprintf(&body, "Defined in `%s:%d`.\n\n@code{.go}\n%s\n@endcode\n\n", relative, line, declaration)
	}
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			name := decl.Name.Name
			if decl.Recv != nil {
				name = expression(fset, decl.Recv.List[0].Type) + "." + name
			}
			// Copy the declaration: filtering must never mutate the parsed body.
			signature := *decl
			signature.Doc, signature.Body = nil, nil
			section(name, comment(decl.Doc), expression(fset, &signature), fset.Position(decl.Pos()).Line)
		case *ast.GenDecl:
			if decl.Tok == token.IMPORT {
				continue
			}
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					doc := comment(spec.Doc)
					if doc == "" {
						doc = comment(decl.Doc)
					}
					section(spec.Name.Name, doc, "type "+expression(fset, spec), fset.Position(spec.Pos()).Line)
				case *ast.ValueSpec:
					// Only documented values are reference material; omit large
					// embedded schemas/prompts and never evaluate initializers.
					doc := comment(spec.Doc)
					if doc == "" {
						doc = comment(decl.Doc)
					}
					if doc != "" {
						names := make([]string, len(spec.Names))
						for i, name := range spec.Names {
							names[i] = name.Name
						}
						section(strings.Join(names, ", "), doc, decl.Tok.String()+" "+expression(fset, spec), fset.Position(spec.Pos()).Line)
					}
				}
			}
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(body.String()), "\n") {
		if _, err := fmt.Fprintln(out, "/// "+line); err != nil {
			return err
		}
	}
	return nil
}

func comment(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	return group.Text()
}

func expression(fset *token.FileSet, node any) string {
	var result bytes.Buffer
	if err := format.Node(&result, fset, node); err != nil {
		return "(declaration unavailable: " + err.Error() + ")"
	}
	return result.String()
}
