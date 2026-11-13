package reviewmap

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
)

// Go extracts changed declarations from Go files with the standard parser.
// Types and const/var specs come from the head parse; a removed declaration
// needs the base parse, because the head no longer describes it.
type Go struct{}

func (Go) Match(path string) bool { return strings.HasSuffix(path, ".go") }

// Names returns the changed declarations in f, then the exported declarations
// f removes. Missing detail degrades rather than failing the map: a head that
// cannot be read or parsed falls back to hunk-header names, and a base that
// cannot be read or parsed leaves the head declarations without removals.
func (Go) Names(f FileDiff, src Source) []Definition {
	// Tests are uses, not contracts: a changed helper in a test file is not a
	// contract a reader must check.
	if strings.HasSuffix(f.Path, "_test.go") {
		return nil
	}
	if src == nil {
		return goHeaderDefinitions(f)
	}
	if f.Deleted {
		base, ok := goParse(f.Path, src.Base)
		if !ok {
			return nil
		}
		return removedGoDefinitions(f.Path, base, nil)
	}

	headData, err := src.Head(f.Path)
	if err != nil {
		return goHeaderDefinitions(f)
	}
	head, ok := goParseBytes(f.Path, headData)
	if !ok {
		return goHeaderDefinitions(f)
	}

	defs := changedGoDefinitions(f, head)
	// A rename's base lives at the source path, not at the head path.
	basePath := f.Path
	if f.RenamedFrom != "" {
		basePath = f.RenamedFrom
	}
	baseData, err := src.Base(basePath)
	if err != nil {
		return defs
	}
	base, ok := goParseBytes(basePath, baseData)
	if !ok {
		return defs
	}
	return append(defs, removedGoDefinitions(basePath, base, head)...)
}

// goDecl is one parsed declaration with its span in the file.
type goDecl struct {
	def   Definition
	start int
	end   int
	// removable is set for exported func and type declarations, the only ones
	// whose removal is a contract break worth reporting.
	removable bool
}

// goParseBytes parses data at path. ParseComments is off: comments never
// carry a declaration's name, and the mode is the zero value.
func goParseBytes(path string, data []byte) ([]goDecl, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return nil, false
	}
	return goDecls(path, fset, f), true
}

// goParse is goParseBytes behind a Source method, whose error carries the
// detail of a missing revision.
func goParse(path string, read func(string) ([]byte, error)) ([]goDecl, bool) {
	data, err := read(path)
	if err != nil {
		return nil, false
	}
	return goParseBytes(path, data)
}

// goDecls collects package-level func and type declarations plus exported
// package-level const and var specs, in source order.
func goDecls(path string, fset *token.FileSet, f *ast.File) []goDecl {
	var decls []goDecl
	line := func(p token.Pos) int { return fset.Position(p).Line }
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if len(name) < minNameLen || goNoiseFunc(name) {
				continue
			}
			recv := goReceiverName(d.Recv)
			display := name
			if recv != "" {
				display = recv + "." + name
			}
			decls = append(decls, goDecl{
				def: Definition{
					Name: display,
					Grep: goGrepName(display, name),
					At:   fmt.Sprintf("%s:%d", path, line(d.Pos())),
				},
				start:     line(d.Pos()),
				end:       line(d.End()),
				removable: ast.IsExported(name),
			})
		case *ast.GenDecl:
			decls = append(decls, goSpecDecls(path, fset, d)...)
		}
	}
	return decls
}

// goSpecDecls collects the type, const and var specs of one GenDecl. Only
// exported package-level const and var names are kept: an unexported one is
// an implementation detail, not a contract.
func goSpecDecls(path string, fset *token.FileSet, d *ast.GenDecl) []goDecl {
	var decls []goDecl
	line := func(p token.Pos) int { return fset.Position(p).Line }
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if len(s.Name.Name) < minNameLen || s.Name.Name == "_" {
				continue
			}
			decls = append(decls, goDecl{
				def: Definition{
					Name: s.Name.Name,
					At:   fmt.Sprintf("%s:%d", path, line(s.Pos())),
				},
				start:     line(s.Pos()),
				end:       line(s.End()),
				removable: ast.IsExported(s.Name.Name),
			})
		case *ast.ValueSpec:
			if d.Tok != token.CONST && d.Tok != token.VAR {
				continue
			}
			for _, n := range s.Names {
				if n.Name == "_" || len(n.Name) < minNameLen || !ast.IsExported(n.Name) {
					continue
				}
				decls = append(decls, goDecl{
					def: Definition{
						Name: n.Name,
						At:   fmt.Sprintf("%s:%d", path, line(n.Pos())),
					},
					start: line(n.Pos()),
					end:   line(s.End()),
				})
			}
		}
	}
	return decls
}

// changedGoDefinitions keeps the head declarations that a hunk overlaps, in
// source order, with the first occurrence of a name winning.
func changedGoDefinitions(f FileDiff, head []goDecl) []Definition {
	var defs []Definition
	seen := make(map[string]bool)
	for _, d := range head {
		if seen[d.def.Name] || !goOverlapsHunk(f, d.start, d.end) {
			continue
		}
		seen[d.def.Name] = true
		defs = append(defs, d.def)
	}
	return defs
}

// goOverlapsHunk reports whether any hunk's head range touches start..end.
// Every added line lies inside its own hunk's range, so the range test alone
// decides: a deletion-only hunk still marks the declaration it covers.
func goOverlapsHunk(f FileDiff, start, end int) bool {
	for _, h := range f.Hunks {
		hunkStart := h.Start
		hunkEnd := h.Start + max(h.Count, 1) - 1
		if hunkStart <= end && start <= hunkEnd {
			return true
		}
	}
	return false
}

// removedGoDefinitions reports exported func and type declarations the base
// version has and the head version does not. A deletion is only worth an item
// when a use survives at head, which the caller decides.
func removedGoDefinitions(path string, base, head []goDecl) []Definition {
	headNames := make(map[string]bool, len(head))
	for _, d := range head {
		headNames[d.def.Name] = true
	}
	var defs []Definition
	seen := make(map[string]bool)
	for _, d := range base {
		if !d.removable || headNames[d.def.Name] || seen[d.def.Name] {
			continue
		}
		seen[d.def.Name] = true
		defs = append(defs, Definition{
			Name:    d.def.Name,
			Grep:    d.def.Grep,
			At:      path,
			Removed: true,
		})
	}
	return defs
}

// goHeaderFuncRE reads the declaration name from a Go hunk header, which git
// sets to the enclosing declaration's first line: "func (d *driver) pick(...)"
// or "func Load(...)". The receiver, when present, is the first group.
var goHeaderFuncRE = regexp.MustCompile(`^\s*func\s*(?:\(([^)]*)\))?\s*([A-Za-z_]\w*)`)

// goHeaderDefinitions is the parse-failure fallback: the hunk headers still
// name the enclosing declarations even when the file does not parse.
func goHeaderDefinitions(f FileDiff) []Definition {
	if f.Deleted {
		return nil
	}
	var defs []Definition
	seen := make(map[string]bool)
	for _, h := range f.Hunks {
		m := goHeaderFuncRE.FindStringSubmatch(h.Context)
		if m == nil {
			continue
		}
		name := m[2]
		if len(name) < minNameLen || goNoiseFunc(name) {
			continue
		}
		display := name
		if recv := goReceiverBase(m[1]); recv != "" {
			display = recv + "." + name
		}
		if seen[display] {
			continue
		}
		seen[display] = true
		defs = append(defs, Definition{
			Name: display,
			Grep: goGrepName(display, name),
			At:   fmt.Sprintf("%s:%d (body changed)", f.Path, h.Start),
		})
	}
	return defs
}

// goReceiverName returns a method receiver's base type name: *driver and
// *List[T] become driver and List.
func goReceiverName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	typ := recv.List[0].Type
	for {
		switch t := typ.(type) {
		case *ast.StarExpr:
			typ = t.X
		case *ast.ParenExpr:
			typ = t.X
		case *ast.IndexExpr:
			typ = t.X
		case *ast.IndexListExpr:
			typ = t.X
		case *ast.Ident:
			if token.IsIdentifier(t.Name) {
				return t.Name
			}
			return ""
		default:
			return ""
		}
	}
}

// goReceiverBase parses a hunk header's receiver text, e.g. "d *driver".
func goReceiverBase(recv string) string {
	fields := strings.Fields(recv)
	if len(fields) == 0 {
		return ""
	}
	typ := strings.TrimPrefix(fields[len(fields)-1], "*")
	if i := strings.IndexByte(typ, '['); i >= 0 {
		typ = typ[:i]
	}
	if !token.IsIdentifier(typ) {
		return ""
	}
	return typ
}

// goGrepName is the bare name git grep needs when the display name is
// qualified by a receiver.
func goGrepName(display, name string) string {
	if display != name {
		return name
	}
	return ""
}

// goFuncNoise drops functions whose names say nothing about a changed
// contract: entry points, the String/Error interface pair, and the test
// function families. Tests are uses, not contracts. The shared method names
// are here because they collide with unrelated namesakes in every package, so
// their use lists would hide the real call sites rather than show them.
var goFuncNoise = map[string]bool{
	"init": true, "main": true, "String": true, "Error": true,
	"Head": true, "Base": true, "Match": true, "Names": true, "show": true,
}

func goNoiseFunc(name string) bool {
	if goFuncNoise[name] {
		return true
	}
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
