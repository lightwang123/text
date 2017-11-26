// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	fmtparser "golang.org/x/text/internal/format"
	"golang.org/x/tools/go/loader"
)

// TODO:
// - merge information into existing files
// - handle different file formats (PO, XLIFF)
// - handle features (gender, plural)
// - message rewriting

var cmdExtract = &Command{
	Run:       runExtract,
	UsageLine: "extract <package>*",
	Short:     "extract strings to be translated from code",
}

func runExtract(cmd *Command, args []string) error {
	if len(args) == 0 {
		args = []string{"."}
	}

	conf := loader.Config{
		Build:      &build.Default,
		ParserMode: parser.ParseComments,
	}

	// Use the initial packages from the command line.
	args, err := conf.FromArgs(args, false)
	if err != nil {
		return err
	}

	// Load, parse and type-check the whole program.
	iprog, err := conf.Load()
	if err != nil {
		return err
	}

	// print returns Go syntax for the specified node.
	print := func(n ast.Node) string {
		var buf bytes.Buffer
		format.Node(&buf, conf.Fset, n)
		return buf.String()
	}

	var messages []Message

	for _, info := range iprog.InitialPackages() {
		for _, f := range info.Files {
			// Associate comments with nodes.
			cmap := ast.NewCommentMap(iprog.Fset, f, f.Comments)
			getComment := func(n ast.Node) string {
				cs := cmap.Filter(n).Comments()
				if len(cs) > 0 {
					return strings.TrimSpace(cs[0].Text())
				}
				return ""
			}

			// Find function calls.
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				// Skip calls of functions other than
				// (*message.Printer).{Sp,Fp,P}rintf.
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				meth := info.Selections[sel]
				if meth == nil || meth.Kind() != types.MethodVal {
					return true
				}
				// TODO: remove cheap hack and check if the type either
				// implements some interface or is specifically of type
				// "golang.org/x/text/message".Printer.
				m, ok := extractFuncs[path.Base(meth.Recv().String())]
				if !ok {
					return true
				}

				fmtType, ok := m[meth.Obj().Name()]
				if !ok {
					return true
				}
				// argn is the index of the format string.
				argn := fmtType.arg
				if argn >= len(call.Args) {
					return true
				}

				args := call.Args[fmtType.arg:]

				fmtMsg, ok := msgStr(info, args[0])
				if !ok {
					// TODO: identify the type of the format argument. If it
					// is not a string, multiple keys may be defined.
					return true
				}
				comment := ""
				key := []string{}
				if ident, ok := args[0].(*ast.Ident); ok {
					key = append(key, ident.Name)
					if v, ok := ident.Obj.Decl.(*ast.ValueSpec); ok && v.Comment != nil {
						// TODO: get comment above ValueSpec as well
						comment = v.Comment.Text()
					}
				}

				key = append(key, fmtMsg)
				arguments := []argument{}
				args = args[1:]
				simArgs := make([]interface{}, len(args))
				for i, arg := range args {
					expr := print(arg)
					val := ""
					if v := info.Types[arg].Value; v != nil {
						val = v.ExactString()
						simArgs[i] = val
						switch arg.(type) {
						case *ast.BinaryExpr, *ast.UnaryExpr:
							expr = val
						}
					}
					arguments = append(arguments, argument{
						ArgNum:         i + 1,
						Type:           info.Types[arg].Type.String(),
						UnderlyingType: info.Types[arg].Type.Underlying().String(),
						Expr:           expr,
						Value:          val,
						Comment:        getComment(arg),
						Position:       posString(conf, info, arg.Pos()),
						// TODO report whether it implements
						// interfaces plural.Interface,
						// gender.Interface.
					})
				}
				msg := ""

				ph := placeholders{index: map[string]string{}}

				p := fmtparser.Parser{}
				p.Reset(simArgs)
				for p.SetFormat(fmtMsg); p.Scan(); {
					switch p.Status {
					case fmtparser.StatusText:
						msg += p.Text()
					case fmtparser.StatusSubstitution,
						fmtparser.StatusBadWidthSubstitution,
						fmtparser.StatusBadPrecSubstitution:
						arguments[p.ArgNum-1].used = true
						arg := arguments[p.ArgNum-1]
						sub := p.Text()
						if !p.HasIndex {
							r, sz := utf8.DecodeLastRuneInString(sub)
							sub = fmt.Sprintf("%s[%d]%c", sub[:len(sub)-sz], p.ArgNum, r)
						}
						msg += fmt.Sprintf("{%s}", ph.addArg(&arg, sub))
					}
				}

				// Add additional Placeholders that can be used in translations
				// that are not present in the string.
				for _, arg := range arguments {
					if arg.used {
						continue
					}
					ph.addArg(&arg, fmt.Sprintf("%%[%d]v", arg.ArgNum))
				}

				if c := getComment(call.Args[0]); c != "" {
					comment = c
				}

				messages = append(messages, Message{
					Key:     key,
					Message: Text{Msg: msg},
					// TODO(fix): this doesn't get the before comment.
					Comment:      comment,
					Placeholders: ph.slice,
					Position:     posString(conf, info, call.Lparen),
				})
				return true
			})
		}
	}

	data, err := json.MarshalIndent(messages, "", "    ")
	if err != nil {
		return err
	}
	for _, tag := range getLangs() {
		// TODO: merge with existing files, don't overwrite.
		os.MkdirAll(*dir, 0744)
		file := filepath.Join(*dir, fmt.Sprintf("gotext_%v.out.json", tag))
		if err := ioutil.WriteFile(file, data, 0744); err != nil {
			return fmt.Errorf("could not create file: %v", err)
		}
	}
	return nil
}

func posString(conf loader.Config, info *loader.PackageInfo, pos token.Pos) string {
	p := conf.Fset.Position(pos)
	file := fmt.Sprintf("%s:%d:%d", filepath.Base(p.Filename), p.Line, p.Column)
	return filepath.Join(info.Pkg.Path(), file)
}

// extractFuncs indicates the types and methods for which to extract strings,
// and which argument to extract.
// TODO: use the types in conf.Import("golang.org/x/text/message") to extract
// the correct instances.
var extractFuncs = map[string]map[string]extractType{
	// TODO: Printer -> *golang.org/x/text/message.Printer
	"message.Printer": {
		"Printf":  extractType{arg: 0, format: true},
		"Sprintf": extractType{arg: 0, format: true},
		"Fprintf": extractType{arg: 1, format: true},

		"Lookup": extractType{arg: 0},
	},
}

type extractType struct {
	// format indicates if the next arg is a formatted string or whether to
	// concatenate all arguments
	format bool
	// arg indicates the position of the argument to extract.
	arg int
}

func getID(arg *argument) string {
	s := getLastComponent(arg.Expr)
	s = strings.Replace(s, " ", "", -1)
	// For small variable names, use user-defined types for more info.
	if len(s) <= 2 && arg.UnderlyingType != arg.Type {
		s = getLastComponent(arg.Type)
	}
	return strings.Title(s)
}

type placeholders struct {
	index map[string]string
	slice []Placeholder
}

func (p *placeholders) addArg(arg *argument, sub string) (id string) {
	id = getID(arg)
	id1 := id
	alt, ok := p.index[id1]
	for i := 1; ok && alt != sub; i++ {
		id1 = fmt.Sprintf("%s_%d", id, i)
		alt, ok = p.index[id1]
	}
	p.index[id1] = sub
	p.slice = append(p.slice, Placeholder{
		ID:             id1,
		String:         sub,
		Type:           arg.Type,
		UnderlyingType: arg.UnderlyingType,
		ArgNum:         arg.ArgNum,
		Expr:           arg.Expr,
		Comment:        arg.Comment,
	})
	return id1
}

func getLastComponent(s string) string {
	return s[1+strings.LastIndexByte(s, '.'):]
}

func msgStr(info *loader.PackageInfo, e ast.Expr) (s string, ok bool) {
	v := info.Types[e].Value
	if v == nil || v.Kind() != constant.String {
		return "", false
	}
	s = constant.StringVal(v)
	// Only record strings with letters.
	for _, r := range s {
		if unicode.In(r, unicode.L) {
			return s, true
		}
	}
	return "", false
}
