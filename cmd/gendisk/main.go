// gendisk generates explicit little-endian MarshalBinary/UnmarshalBinary methods
// for Go structs that mirror the BrieFS on-disk layout.
//
// It is intended to be invoked from the briefs package via:
//
//	//go:generate go run ../cmd/gendisk -out gen_disk.go -dir .
//
// Only structs bearing a "//go:briefs-disk" marker comment are processed.
// The generator supports uint8/16/32/64 scalar fields, fixed-size byte arrays,
// and fixed-size arrays of other disk structs. All generated code is in the
// same package as the source types, so unexported fields are accessible.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

var sizeRe = regexp.MustCompile(`size\s*=\s*(\d+)`)

type fieldInfo struct {
	Name string
	Type ast.Expr
	Tag  string
}

type typeInfo struct {
	Name   string
	Fields []fieldInfo
	// ExpectedSize, if non-zero, is the exact byte size asserted at compile time.
	ExpectedSize int64
	// Packed is true for structs marked "packed": their on-disk layout has
	// unaligned fields (Go has no packed structs), so the generated Size()
	// returns ExpectedSize directly instead of unsafe.Sizeof, and the size
	// is verified at generation time by summing field widths rather than via
	// the unsafe.Sizeof compile-time assertion.
	Packed bool
}

// diskGen holds the generator state for a package.
type diskGen struct {
	consts      map[string]int64
	structNames map[string]struct{}
}

func main() {
	out := flag.String("out", "gen_disk.go", "output file path")
	dir := flag.String("dir", ".", "directory containing the source package")
	flag.Parse()

	gen, diskTypes, err := parsePackage(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendisk: parse package: %v\n", err)
		os.Exit(1)
	}

	if len(diskTypes) == 0 {
		fmt.Fprintf(os.Stderr, "gendisk: no structs marked with //go:briefs-disk found\n")
		os.Exit(1)
	}

	if err := generate(*out, diskTypes, gen); err != nil {
		fmt.Fprintf(os.Stderr, "gendisk: generate: %v\n", err)
		os.Exit(1)
	}
}

// findSize scans a comment group for "size=NNN" and returns the value.
func findSize(docs ...*ast.CommentGroup) int64 {
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for _, line := range doc.List {
			if m := sizeRe.FindStringSubmatch(line.Text); m != nil {
				v, _ := strconv.ParseInt(m[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

// findPacked reports whether any marker comment line contains the "packed"
// token (alongside the //go:briefs-disk marker). Packed structs have on-disk
// layouts with unaligned fields that Go's struct alignment cannot represent,
// so their Size() is the declared size, not unsafe.Sizeof.
func findPacked(docs ...*ast.CommentGroup) bool {
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for _, line := range doc.List {
			if !strings.Contains(line.Text, "go:briefs-disk") {
				continue
			}
			if strings.Contains(line.Text, "packed") {
				return true
			}
		}
	}
	return false
}

func parsePackage(dir string) (*diskGen, []typeInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}

	fset := token.NewFileSet()
	var files []*ast.File
	gen := &diskGen{
		consts:      make(map[string]int64),
		structNames: make(map[string]struct{}),
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		files = append(files, f)

		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					gen.consts[ident.Name] = evalConstExpr(vs.Values[i], gen.consts)
				}
			}
		}
	}

	var diskTypes []typeInfo
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if ok {
					gen.structNames[ts.Name.Name] = struct{}{}
				}
				if !hasMarker(gd.Doc, ts.Doc) {
					continue
				}
				if !ok {
					continue
				}
				var fields []fieldInfo
				for _, fld := range st.Fields.List {
					tag := ""
					if fld.Tag != nil {
						tag = strings.Trim(fld.Tag.Value, "`")
					}
					if len(fld.Names) == 0 {
						// Skip embedded fields; none are used in BrieFS disk structs.
						continue
					}
					for _, name := range fld.Names {
						fields = append(fields, fieldInfo{
							Name: name.Name,
							Type: fld.Type,
							Tag:  tag,
						})
					}
				}
				diskTypes = append(diskTypes, typeInfo{
					Name:         ts.Name.Name,
					Fields:       fields,
					ExpectedSize: findSize(gd.Doc, ts.Doc),
					Packed:       findPacked(gd.Doc, ts.Doc),
				})
			}
		}
	}

	sort.Slice(diskTypes, func(i, j int) bool { return diskTypes[i].Name < diskTypes[j].Name })
	return gen, diskTypes, nil
}

func hasMarker(docs ...*ast.CommentGroup) bool {
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for _, line := range doc.List {
			if strings.Contains(line.Text, "go:briefs-disk") {
				return true
			}
		}
	}
	return false
}

func evalConstExpr(expr ast.Expr, consts map[string]int64) int64 {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.INT {
			v, err := strconv.ParseInt(e.Value, 0, 64)
			if err != nil {
				return 0
			}
			return v
		}
	case *ast.Ident:
		return consts[e.Name]
	case *ast.BinaryExpr:
		l := evalConstExpr(e.X, consts)
		r := evalConstExpr(e.Y, consts)
		switch e.Op {
		case token.ADD:
			return l + r
		case token.SUB:
			return l - r
		case token.MUL:
			return l * r
		case token.QUO:
			if r == 0 {
				return 0
			}
			return l / r
		case token.SHL:
			return l << uint(r)
		case token.SHR:
			return l >> uint(r)
		case token.AND:
			return l & r
		case token.OR:
			return l | r
		case token.XOR:
			return l ^ r
		}
	case *ast.ParenExpr:
		return evalConstExpr(e.X, consts)
	case *ast.UnaryExpr:
		v := evalConstExpr(e.X, consts)
		switch e.Op {
		case token.ADD:
			return v
		case token.SUB:
			return -v
		case token.NOT:
			return ^v
		}
	}
	return 0
}

func generate(out string, types []typeInfo, gen *diskGen) error {
	var buf bytes.Buffer
	buf.WriteString(`// Code generated by cmd/gendisk. DO NOT EDIT.

package briefs

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

`)

	for _, t := range types {
		if err := writeType(&buf, t, gen); err != nil {
			return err
		}
	}

	return os.WriteFile(out, buf.Bytes(), 0644)
}

func writeType(buf *bytes.Buffer, t typeInfo, gen *diskGen) error {
	// Packed structs have no unsafe.Sizeof to lean on (Go has no packed
	// attribute; unaligned fields sit at aligned Go offsets). Verify the
	// field-width sum equals the declared size at generation time so a
	// layout/size mismatch fails `go generate` instead of silently emitting
	// a wrong codec.
	if t.Packed && t.ExpectedSize != 0 {
		sum, err := structWidth(t.Fields, gen)
		if err != nil {
			return fmt.Errorf("packed size check for %s: %w", t.Name, err)
		}
		if sum != t.ExpectedSize {
			return fmt.Errorf("packed struct %s: field-width sum %d != declared size %d", t.Name, sum, t.ExpectedSize)
		}
	}

	d := map[string]interface{}{
		"Name":       t.Name,
		"Fields":     t.Fields,
		"SizeMethod": sizeMethodString(t),
		"SizeAssert": sizeAssertString(t),
	}

	funcMap := template.FuncMap{
		"marshalField":   func(f fieldInfo) (string, error) { return marshalField(f, gen) },
		"unmarshalField": func(f fieldInfo) (string, error) { return unmarshalField(f, gen) },
	}

	tmpl := template.Must(template.New("type").Funcs(funcMap).Parse(typeTmpl))
	return tmpl.Execute(buf, d)
}

// sizeMethodString renders the Size() method for a struct. Packed structs have
// on-disk layouts with unaligned fields that Go's struct alignment cannot
// represent, so unsafe.Sizeof would be wrong; their Size() returns the declared
// size (verified at generation time by the field-width sum in writeType).
// Everything else uses unsafe.Sizeof, which the compile-time assertion checks.
func sizeMethodString(t typeInfo) string {
	if t.Packed {
		return fmt.Sprintf("// Size returns the on-disk size of %s. Packed structs have\n"+
			"// unaligned on-disk fields that Go's struct alignment cannot represent, so\n"+
			"// this is the declared size, not unsafe.Sizeof (the field-width sum is\n"+
			"// verified at generation time).\n"+
			"func (s *%s) Size() int { return %d }", t.Name, t.Name, t.ExpectedSize)
	}
	return fmt.Sprintf("// Size returns the on-disk size of %s.\n"+
		"func (s *%s) Size() int { return int(unsafe.Sizeof(%s{})) }", t.Name, t.Name, t.Name)
}

// sizeAssertString renders the trailing size guardrail: a compile-time
// unsafe.Sizeof assertion for naturally-aligned structs, a generation-time
// field-width-sum note for packed structs, or a bare separator newline when no
// size is declared. The leading and trailing newlines position the block as a
// blank-line-separated trailer after the UnmarshalBinary close brace.
func sizeAssertString(t typeInfo) string {
	switch {
	case t.ExpectedSize != 0 && !t.Packed:
		return fmt.Sprintf("\n// Compile-time size assertion for %s.\n"+
			"var _ = [1]struct{}{}[unsafe.Sizeof(%s{}) - %d]\n\n", t.Name, t.Name, t.ExpectedSize)
	case t.ExpectedSize != 0 && t.Packed:
		return fmt.Sprintf("\n// Packed layout: %d bytes (field-width sum verified at generation time).\n\n",
			t.ExpectedSize)
	default:
		return "\n"
	}
}

// structWidth sums the on-disk byte width of every field (skipping disk:"-"
// fields). It mirrors the sequential pos increments the generated marshal
// emits, so for a packed struct it equals the true on-disk size.
func structWidth(fields []fieldInfo, gen *diskGen) (int64, error) {
	var sum int64
	for _, f := range fields {
		w, err := fieldWidth(f, gen)
		if err != nil {
			return 0, err
		}
		sum += w
	}
	return sum, nil
}

// fieldWidth returns the on-disk byte width of a single field.
func fieldWidth(f fieldInfo, gen *diskGen) (int64, error) {
	if hasTag(f.Tag, "disk", "-") {
		return 0, nil
	}
	if kind := scalarKind(f.Type); kind != "" {
		return int64(scalarWidth(kind)), nil
	}
	if n, ok := byteArrayLen(f.Type, gen.consts); ok {
		return n, nil
	}
	if elemKind, ok := scalarArrayElem(f.Type); ok {
		arr := f.Type.(*ast.ArrayType)
		n := evalConstExpr(arr.Len, gen.consts)
		return n * int64(scalarWidth(elemKind)), nil
	}
	if elem, ok := structArrayElem(f.Type); ok {
		// No current packed struct uses a struct-array field. If one is
		// added, extend this to compute the element's own field-width sum.
		_ = elem
		return 0, fmt.Errorf("struct array field %s: packed sizing not implemented", f.Name)
	}
	return 0, fmt.Errorf("unsupported field type for %s", f.Name)
}

const typeTmpl = `{{.SizeMethod}}

// MarshalBinary serializes {{.Name}} to its little-endian on-disk representation.
func (s *{{.Name}}) MarshalBinary() ([]byte, error) {
	data := make([]byte, s.Size())
	pos := 0
{{range .Fields}}	{{marshalField .}}
{{end}}	return data, nil
}

// UnmarshalBinary deserializes {{.Name}} from its little-endian on-disk representation.
func (s *{{.Name}}) UnmarshalBinary(data []byte) error {
	if len(data) < s.Size() {
		return fmt.Errorf("{{.Name}} data too short: %d < %d", len(data), s.Size())
	}
	pos := 0
{{range .Fields}}	{{unmarshalField .}}
{{end}}	return nil
}
{{.SizeAssert}}`

func marshalField(f fieldInfo, gen *diskGen) (string, error) {
	if hasTag(f.Tag, "disk", "-") {
		return "// field " + f.Name + " skipped by disk:\"-\" tag", nil
	}

	// Scalar types.
	if kind := scalarKind(f.Type); kind != "" {
		switch kind {
		case "uint8":
			return fmt.Sprintf("data[pos] = s.%s; pos += 1", f.Name), nil
		case "uint16":
			return fmt.Sprintf("binary.LittleEndian.PutUint16(data[pos:], s.%s); pos += 2", f.Name), nil
		case "uint32":
			return fmt.Sprintf("binary.LittleEndian.PutUint32(data[pos:], s.%s); pos += 4", f.Name), nil
		case "uint64":
			return fmt.Sprintf("binary.LittleEndian.PutUint64(data[pos:], s.%s); pos += 8", f.Name), nil
		}
	}

	// Fixed-size byte/uint8 array: copy the whole slice.
	if n, ok := byteArrayLen(f.Type, gen.consts); ok {
		return fmt.Sprintf("copy(data[pos:], s.%s[:]); pos += %d", f.Name, n), nil
	}

	// Fixed-size array of scalar types (e.g. [4]uint64).
	if elemKind, ok := scalarArrayElem(f.Type); ok {
		if elemKind == "uint8" {
			return fmt.Sprintf("copy(data[pos:], s.%s[:]); pos += len(s.%s)", f.Name, f.Name), nil
		}
		width := scalarWidth(elemKind)
		putter := scalarPutter(elemKind)
		return fmt.Sprintf("for i := 0; i < len(s.%s); i++ {\n\t\t%s s.%s[i]); pos += %d\n\t}", f.Name, putter, f.Name, width), nil
	}

	// Fixed-size array of disk structs: loop and call generated methods.
	if elem, ok := structArrayElem(f.Type); ok {
		if _, isStruct := gen.structNames[elem]; isStruct {
			return fmt.Sprintf("for i := 0; i < len(s.%s); i++ {\n\t\tsub, err := s.%s[i].MarshalBinary()\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tcopy(data[pos:], sub)\n\t\tpos += len(sub)\n\t}", f.Name, f.Name), nil
		}
	}

	return "", fmt.Errorf("unsupported field type for %s", f.Name)
}

func unmarshalField(f fieldInfo, gen *diskGen) (string, error) {
	if hasTag(f.Tag, "disk", "-") {
		return "// field " + f.Name + " skipped by disk:\"-\" tag", nil
	}

	if kind := scalarKind(f.Type); kind != "" {
		switch kind {
		case "uint8":
			return fmt.Sprintf("s.%s = data[pos]; pos += 1", f.Name), nil
		case "uint16":
			return fmt.Sprintf("s.%s = binary.LittleEndian.Uint16(data[pos:]); pos += 2", f.Name), nil
		case "uint32":
			return fmt.Sprintf("s.%s = binary.LittleEndian.Uint32(data[pos:]); pos += 4", f.Name), nil
		case "uint64":
			return fmt.Sprintf("s.%s = binary.LittleEndian.Uint64(data[pos:]); pos += 8", f.Name), nil
		}
	}

	if n, ok := byteArrayLen(f.Type, gen.consts); ok {
		return fmt.Sprintf("copy(s.%s[:], data[pos:pos+%d]); pos += %d", f.Name, n, n), nil
	}

	if elemKind, ok := scalarArrayElem(f.Type); ok {
		width := scalarWidth(elemKind)
		getter := scalarGetter(elemKind)
		return fmt.Sprintf("for i := 0; i < len(s.%s); i++ {\n\t\ts.%s[i] = %s; pos += %d\n\t}", f.Name, f.Name, getter, width), nil
	}

	if elem, ok := structArrayElem(f.Type); ok {
		if _, isStruct := gen.structNames[elem]; isStruct {
			return fmt.Sprintf("for i := 0; i < len(s.%s); i++ {\n\t\tif err := s.%s[i].UnmarshalBinary(data[pos:]); err != nil {\n\t\t\treturn err\n\t\t}\n\t\tpos += s.%s[i].Size()\n\t}", f.Name, f.Name, f.Name), nil
		}
	}

	return "", fmt.Errorf("unsupported field type for %s", f.Name)
}

func scalarArrayElem(t ast.Expr) (string, bool) {
	arr, ok := t.(*ast.ArrayType)
	if !ok {
		return "", false
	}
	ident, ok := arr.Elt.(*ast.Ident)
	if !ok {
		return "", false
	}
	if scalarKind(ident) == "" {
		return "", false
	}
	return ident.Name, true
}

func scalarWidth(kind string) int {
	switch kind {
	case "uint8":
		return 1
	case "uint16":
		return 2
	case "uint32":
		return 4
	case "uint64":
		return 8
	}
	return 0
}

func scalarPutter(kind string) string {
	switch kind {
	case "uint8":
		return "data[pos] ="
	case "uint16":
		return "binary.LittleEndian.PutUint16(data[pos:],"
	case "uint32":
		return "binary.LittleEndian.PutUint32(data[pos:],"
	case "uint64":
		return "binary.LittleEndian.PutUint64(data[pos:],"
	}
	return ""
}

func scalarGetter(kind string) string {
	switch kind {
	case "uint8":
		return "data[pos]"
	case "uint16":
		return "binary.LittleEndian.Uint16(data[pos:])"
	case "uint32":
		return "binary.LittleEndian.Uint32(data[pos:])"
	case "uint64":
		return "binary.LittleEndian.Uint64(data[pos:])"
	}
	return ""
}

func scalarKind(t ast.Expr) string {
	ident, ok := t.(*ast.Ident)
	if !ok {
		return ""
	}
	switch ident.Name {
	case "uint8", "uint16", "uint32", "uint64":
		return ident.Name
	}
	return ""
}

func isByteType(t ast.Expr) bool {
	ident, ok := t.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == "byte" || ident.Name == "uint8"
}

func byteArrayLen(t ast.Expr, consts map[string]int64) (int64, bool) {
	arr, ok := t.(*ast.ArrayType)
	if !ok {
		return 0, false
	}
	if !isByteType(arr.Elt) {
		return 0, false
	}
	if arr.Len == nil {
		return 0, false
	}
	return evalConstExpr(arr.Len, consts), true
}

func structArrayElem(t ast.Expr) (string, bool) {
	arr, ok := t.(*ast.ArrayType)
	if !ok {
		return "", false
	}
	ident, ok := arr.Elt.(*ast.Ident)
	if !ok {
		return "", false
	}
	return ident.Name, true
}

func hasTag(tag, key, value string) bool {
	return strings.Contains(tag, fmt.Sprintf(`%s:"%s"`, key, value))
}
