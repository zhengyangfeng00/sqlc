package kotlin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/kyleconroy/sqlc/internal/codegen/sdk"
	"github.com/kyleconroy/sqlc/internal/inflection"
	"github.com/kyleconroy/sqlc/internal/metadata"
	"github.com/kyleconroy/sqlc/internal/plugin"
)

var ktIdentPattern = regexp.MustCompile("[^a-zA-Z0-9_]+")

type Constant struct {
	Name  string
	Type  string
	Value string
}

type Enum struct {
	Name      string
	Comment   string
	Constants []Constant
}

type Field struct {
	Name    string
	Type    ktType
	Comment string
}

type Struct struct {
	Table             plugin.Identifier
	Name              string
	Fields            []Field
	JDBCParamBindings []Field
	Comment           string
}

type QueryValue struct {
	Emit               bool
	Name               string
	Struct             *Struct
	Typ                ktType
	JDBCParamBindCount int
}

func (v QueryValue) EmitStruct() bool {
	return v.Emit
}

func (v QueryValue) IsStruct() bool {
	return v.Struct != nil
}

func (v QueryValue) isEmpty() bool {
	return v.Typ == (ktType{}) && v.Name == "" && v.Struct == nil
}

func (v QueryValue) Type() string {
	if v.Typ != (ktType{}) {
		return v.Typ.String()
	}
	if v.Struct != nil {
		return v.Struct.Name
	}
	panic("no type for QueryValue: " + v.Name)
}

func jdbcSet(t ktType, idx int, name string) string {
	if t.IsEnum && t.IsArray {
		return fmt.Sprintf(`stmt.setArray(%d, conn.createArrayOf("%s", %s.map { v -> v.value }.toTypedArray()))`, idx, t.DataType, name)
	}
	if t.IsEnum {
		if t.Engine == "postgresql" {
			return fmt.Sprintf("stmt.setObject(%d, %s.value, %s)", idx, name, "Types.OTHER")
		} else {
			return fmt.Sprintf("stmt.setString(%d, %s.value)", idx, name)
		}
	}
	if t.IsArray {
		return fmt.Sprintf(`stmt.setArray(%d, conn.createArrayOf("%s", %s.toTypedArray()))`, idx, t.DataType, name)
	}
	if t.IsTime() {
		return fmt.Sprintf("stmt.setObject(%d, %s)", idx, name)
	}
	if t.IsInstant() {
		return fmt.Sprintf("stmt.setTimestamp(%d, Timestamp.from(%s))", idx, name)
	}
	if t.IsUUID() {
		return fmt.Sprintf("stmt.setObject(%d, %s)", idx, name)
	}
	return fmt.Sprintf("stmt.set%s(%d, %s)", t.Name, idx, name)
}

type Params struct {
	Struct *Struct
}

func (v Params) isEmpty() bool {
	return len(v.Struct.Fields) == 0
}

func (v Params) Args() string {
	if v.isEmpty() {
		return ""
	}
	var out []string
	for _, f := range v.Struct.Fields {
		out = append(out, f.Name+": "+f.Type.String())
	}
	if len(out) < 3 {
		return strings.Join(out, ", ")
	}
	return "\n" + indent(strings.Join(out, ",\n"), 6, -1)
}

func (v Params) Bindings() string {
	if v.isEmpty() {
		return ""
	}
	var out []string
	for i, f := range v.Struct.JDBCParamBindings {
		out = append(out, jdbcSet(f.Type, i+1, f.Name))
	}
	return indent(strings.Join(out, "\n"), 10, 0)
}

func jdbcGet(t ktType, idx int) string {
	if t.IsEnum && t.IsArray {
		return fmt.Sprintf(`(results.getArray(%d).array as Array<String>).map { v -> %s.lookup(v)!! }.toList()`, idx, t.Name)
	}
	if t.IsEnum {
		return fmt.Sprintf("%s.lookup(results.getString(%d))!!", t.Name, idx)
	}
	if t.IsArray {
		return fmt.Sprintf(`(results.getArray(%d).array as Array<%s>).toList()`, idx, t.Name)
	}
	if t.IsTime() {
		return fmt.Sprintf(`results.getObject(%d, %s::class.java)`, idx, t.Name)
	}
	if t.IsInstant() {
		return fmt.Sprintf(`results.getTimestamp(%d).toInstant()`, idx)
	}
	if t.IsUUID() {
		var nullCast string
		if t.IsNull {
			nullCast = "?"
		}
		return fmt.Sprintf(`results.getObject(%d) as%s %s`, idx, nullCast, t.Name)
	}
	return fmt.Sprintf(`results.get%s(%d)`, t.Name, idx)
}

func (v QueryValue) ResultSet() string {
	var out []string
	if v.Struct == nil {
		return jdbcGet(v.Typ, 1)
	}
	for i, f := range v.Struct.Fields {
		out = append(out, jdbcGet(f.Type, i+1))
	}
	ret := indent(strings.Join(out, ",\n"), 4, -1)
	ret = indent(v.Struct.Name+"(\n"+ret+"\n)", 12, 0)
	return ret
}

func indent(s string, n int, firstIndent int) string {
	lines := strings.Split(s, "\n")
	buf := bytes.NewBuffer(nil)
	for i, l := range lines {
		indent := n
		if i == 0 && firstIndent != -1 {
			indent = firstIndent
		}
		if i != 0 {
			buf.WriteRune('\n')
		}
		for i := 0; i < indent; i++ {
			buf.WriteRune(' ')
		}
		buf.WriteString(l)
	}
	return buf.String()
}

// A struct used to generate methods and fields on the Queries struct
type Query struct {
	ClassName    string
	Cmd          string
	Comments     []string
	MethodName   string
	FieldName    string
	ConstantName string
	SQL          string
	SourceName   string
	Ret          QueryValue
	Arg          Params
}

func ktEnumValueName(value string) string {
	id := strings.Replace(value, "-", "_", -1)
	id = strings.Replace(id, ":", "_", -1)
	id = strings.Replace(id, "/", "_", -1)
	id = ktIdentPattern.ReplaceAllString(id, "")
	return strings.ToUpper(id)
}

func buildEnums(req *plugin.CodeGenRequest) []Enum {
	var enums []Enum
	for _, schema := range req.Catalog.Schemas {
		if schema.Name == "pg_catalog" || schema.Name == "information_schema" {
			continue
		}
		for _, enum := range schema.Enums {
			var enumName string
			if schema.Name == req.Catalog.DefaultSchema {
				enumName = enum.Name
			} else {
				enumName = schema.Name + "_" + enum.Name
			}
			e := Enum{
				Name:    dataClassName(enumName, req.Settings),
				Comment: enum.Comment,
			}
			for _, v := range enum.Vals {
				e.Constants = append(e.Constants, Constant{
					Name:  ktEnumValueName(v),
					Value: v,
					Type:  e.Name,
				})
			}
			enums = append(enums, e)
		}
	}
	if len(enums) > 0 {
		sort.Slice(enums, func(i, j int) bool { return enums[i].Name < enums[j].Name })
	}
	return enums
}

func dataClassName(name string, settings *plugin.Settings) string {
	if rename := settings.Rename[name]; rename != "" {
		return rename
	}
	out := ""
	for _, p := range strings.Split(name, "_") {
		out += strings.Title(p)
	}
	return out
}

func memberName(name string, settings *plugin.Settings) string {
	return sdk.LowerTitle(dataClassName(name, settings))
}

func buildDataClasses(req *plugin.CodeGenRequest) []Struct {
	var structs []Struct
	for _, schema := range req.Catalog.Schemas {
		if schema.Name == "pg_catalog" || schema.Name == "information_schema" {
			continue
		}
		for _, table := range schema.Tables {
			var tableName string
			if schema.Name == req.Catalog.DefaultSchema {
				tableName = table.Rel.Name
			} else {
				tableName = schema.Name + "_" + table.Rel.Name
			}
			structName := dataClassName(tableName, req.Settings)
			if !req.Settings.Kotlin.EmitExactTableNames {
				structName = inflection.Singular(inflection.SingularParams{
					Name:       structName,
					Exclusions: req.Settings.Kotlin.InflectionExcludeTableNames,
				})
			}
			s := Struct{
				Table:   plugin.Identifier{Schema: schema.Name, Name: table.Rel.Name},
				Name:    structName,
				Comment: table.Comment,
			}
			for _, column := range table.Columns {
				s.Fields = append(s.Fields, Field{
					Name:    memberName(column.Name, req.Settings),
					Type:    makeType(req, column),
					Comment: column.Comment,
				})
			}
			structs = append(structs, s)
		}
	}
	if len(structs) > 0 {
		sort.Slice(structs, func(i, j int) bool { return structs[i].Name < structs[j].Name })
	}
	return structs
}

type ktType struct {
	Name     string
	IsEnum   bool
	IsArray  bool
	IsNull   bool
	DataType string
	Engine   string
}

func (t ktType) String() string {
	v := t.Name
	if t.IsArray {
		v = fmt.Sprintf("List<%s>", v)
	} else if t.IsNull {
		v += "?"
	}
	return v
}

func (t ktType) jdbcSetter() string {
	return "set" + t.jdbcType()
}

func (t ktType) jdbcType() string {
	if t.IsArray {
		return "Array"
	}
	if t.IsEnum || t.IsTime() {
		return "Object"
	}
	if t.IsInstant() {
		return "Timestamp"
	}
	return t.Name
}

func (t ktType) IsTime() bool {
	return t.Name == "LocalDate" || t.Name == "LocalDateTime" || t.Name == "LocalTime" || t.Name == "OffsetDateTime"
}

func (t ktType) IsInstant() bool {
	return t.Name == "Instant"
}

func (t ktType) IsUUID() bool {
	return t.Name == "UUID"
}

func makeType(req *plugin.CodeGenRequest, col *plugin.Column) ktType {
	typ, isEnum := ktInnerType(req, col)
	return ktType{
		Name:     typ,
		IsEnum:   isEnum,
		IsArray:  col.IsArray,
		IsNull:   !col.NotNull,
		DataType: sdk.DataType(col.Type),
		Engine:   req.Settings.Engine,
	}
}

func ktInnerType(req *plugin.CodeGenRequest, col *plugin.Column) (string, bool) {
	// TODO: Extend the engine interface to handle types
	switch req.Settings.Engine {
	case "mysql":
		return mysqlType(req, col)
	case "postgresql":
		return postgresType(req, col)
	default:
		return "Any", false
	}
}

type goColumn struct {
	id int
	*plugin.Column
}

func ktColumnsToStruct(req *plugin.CodeGenRequest, name string, columns []goColumn, namer func(*plugin.Column, int) string) *Struct {
	gs := Struct{
		Name: name,
	}
	idSeen := map[int]Field{}
	nameSeen := map[string]int{}
	for _, c := range columns {
		if binding, ok := idSeen[c.id]; ok {
			gs.JDBCParamBindings = append(gs.JDBCParamBindings, binding)
			continue
		}
		fieldName := memberName(namer(c.Column, c.id), req.Settings)
		if v := nameSeen[c.Name]; v > 0 {
			fieldName = fmt.Sprintf("%s_%d", fieldName, v+1)
		}
		field := Field{
			Name: fieldName,
			Type: makeType(req, c.Column),
		}
		gs.Fields = append(gs.Fields, field)
		gs.JDBCParamBindings = append(gs.JDBCParamBindings, field)
		nameSeen[c.Name]++
		idSeen[c.id] = field
	}
	return &gs
}

func ktArgName(name string) string {
	out := ""
	for i, p := range strings.Split(name, "_") {
		if i == 0 {
			out += strings.ToLower(p)
		} else {
			out += strings.Title(p)
		}
	}
	return out
}

func ktParamName(c *plugin.Column, number int) string {
	if c.Name != "" {
		return ktArgName(c.Name)
	}
	return fmt.Sprintf("dollar_%d", number)
}

func ktColumnName(c *plugin.Column, pos int) string {
	if c.Name != "" {
		return c.Name
	}
	return fmt.Sprintf("column_%d", pos+1)
}

var postgresPlaceholderRegexp = regexp.MustCompile(`\B\$\d+\b`)

// HACK: jdbc doesn't support numbered parameters, so we need to transform them to question marks...
// But there's no access to the SQL parser here, so we just do a dumb regexp replace instead. This won't work if
// the literal strings contain matching values, but good enough for a prototype.
func jdbcSQL(s, engine string) string {
	if engine == "postgresql" {
		return postgresPlaceholderRegexp.ReplaceAllString(s, "?")
	}
	return s
}

func buildQueries(req *plugin.CodeGenRequest, structs []Struct) ([]Query, error) {
	qs := make([]Query, 0, len(req.Queries))
	for _, query := range req.Queries {
		if query.Name == "" {
			continue
		}
		if query.Cmd == "" {
			continue
		}
		if query.Cmd == metadata.CmdCopyFrom {
			return nil, errors.New("Support for CopyFrom in Kotlin is not implemented")
		}

		gq := Query{
			Cmd:          query.Cmd,
			ClassName:    strings.Title(query.Name),
			ConstantName: sdk.LowerTitle(query.Name),
			FieldName:    sdk.LowerTitle(query.Name) + "Stmt",
			MethodName:   sdk.LowerTitle(query.Name),
			SourceName:   query.Filename,
			SQL:          jdbcSQL(query.Text, req.Settings.Engine),
			Comments:     query.Comments,
		}

		var cols []goColumn
		for _, p := range query.Params {
			cols = append(cols, goColumn{
				id:     int(p.Number),
				Column: p.Column,
			})
		}
		params := ktColumnsToStruct(req, gq.ClassName+"Bindings", cols, ktParamName)
		gq.Arg = Params{
			Struct: params,
		}

		if len(query.Columns) == 1 {
			c := query.Columns[0]
			gq.Ret = QueryValue{
				Name: "results",
				Typ:  makeType(req, c),
			}
		} else if len(query.Columns) > 1 {
			var gs *Struct
			var emit bool

			for _, s := range structs {
				if len(s.Fields) != len(query.Columns) {
					continue
				}
				same := true
				for i, f := range s.Fields {
					c := query.Columns[i]
					sameName := f.Name == memberName(ktColumnName(c, i), req.Settings)
					sameType := f.Type == makeType(req, c)
					sameTable := sdk.SameTableName(c.Table, &s.Table, req.Catalog.DefaultSchema)

					if !sameName || !sameType || !sameTable {
						same = false
					}
				}
				if same {
					gs = &s
					break
				}
			}

			if gs == nil {
				var columns []goColumn
				for i, c := range query.Columns {
					columns = append(columns, goColumn{
						id:     i,
						Column: c,
					})
				}
				gs = ktColumnsToStruct(req, gq.ClassName+"Row", columns, ktColumnName)
				emit = true
			}
			gq.Ret = QueryValue{
				Emit:   emit,
				Name:   "results",
				Struct: gs,
			}
		}

		qs = append(qs, gq)
	}
	sort.Slice(qs, func(i, j int) bool { return qs[i].MethodName < qs[j].MethodName })
	return qs, nil
}

var ktIfaceTmpl = `// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc {{.SqlcVersion}}

package {{.Package}}

{{range imports .SourceName}}
{{range .}}import {{.}}
{{end}}
{{end}}

interface Queries {
  {{- range .Queries}}
  @Throws(SQLException::class)
  {{- if eq .Cmd ":one"}}
  fun {{.MethodName}}({{.Arg.Args}}): {{.Ret.Type}}?
  {{- end}}
  {{- if eq .Cmd ":many"}}
  fun {{.MethodName}}({{.Arg.Args}}): List<{{.Ret.Type}}>
  {{- end}}
  {{- if eq .Cmd ":exec"}}
  fun {{.MethodName}}({{.Arg.Args}})
  {{- end}}
  {{- if eq .Cmd ":execrows"}}
  fun {{.MethodName}}({{.Arg.Args}}): Int
  {{- end}}
  {{- if eq .Cmd ":execresult"}}
  fun {{.MethodName}}({{.Arg.Args}}): Long
  {{- end}}
  {{end}}
}
`

var ktModelsTmpl = `// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc {{.SqlcVersion}}

package {{.Package}}

{{range imports .SourceName}}
{{range .}}import {{.}}
{{end}}
{{end}}

{{range .Enums}}
{{if .Comment}}{{comment .Comment}}{{end}}
enum class {{.Name}}(val value: String) {
  {{- range $i, $e := .Constants}}
  {{- if $i }},{{end}}
  {{.Name}}("{{.Value}}")
  {{- end}};

  companion object {
    private val map = {{.Name}}.values().associateBy({{.Name}}::value)
    fun lookup(value: String) = map[value]
  }
}
{{end}}

{{range .DataClasses}}
{{if .Comment}}{{comment .Comment}}{{end}}
data class {{.Name}} ( {{- range $i, $e := .Fields}}
  {{- if $i }},{{end}}
  {{- if .Comment}}
  {{comment .Comment}}{{else}}
  {{- end}}
  val {{.Name}}: {{.Type}}
  {{- end}}
)
{{end}}
`

var ktSqlTmpl = `// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc {{.SqlcVersion}}

package {{.Package}}

{{range imports .SourceName}}
{{range .}}import {{.}}
{{end}}
{{end}}

{{range .Queries}}
const val {{.ConstantName}} = {{$.Q}}-- name: {{.MethodName}} {{.Cmd}}
{{.SQL}}
{{$.Q}}

{{if .Ret.EmitStruct}}
data class {{.Ret.Type}} ( {{- range $i, $e := .Ret.Struct.Fields}}
  {{- if $i }},{{end}}
  val {{.Name}}: {{.Type}}
  {{- end}}
)
{{end}}
{{end}}

class QueriesImpl(private val conn: Connection) : Queries {
{{range .Queries}}
{{if eq .Cmd ":one"}}
{{range .Comments}}//{{.}}
{{end}}
  @Throws(SQLException::class)
  override fun {{.MethodName}}({{.Arg.Args}}): {{.Ret.Type}}? {
    return conn.prepareStatement({{.ConstantName}}).use { stmt ->
      {{.Arg.Bindings}}

      val results = stmt.executeQuery()
      if (!results.next()) {
        return null
      }
      val ret = {{.Ret.ResultSet}}
      if (results.next()) {
          throw SQLException("expected one row in result set, but got many")
      }
      ret
    }
  }
{{end}}

{{if eq .Cmd ":many"}}
{{range .Comments}}//{{.}}
{{end}}
  @Throws(SQLException::class)
  override fun {{.MethodName}}({{.Arg.Args}}): List<{{.Ret.Type}}> {
    return conn.prepareStatement({{.ConstantName}}).use { stmt ->
      {{.Arg.Bindings}}

      val results = stmt.executeQuery()
      val ret = mutableListOf<{{.Ret.Type}}>()
      while (results.next()) {
          ret.add({{.Ret.ResultSet}})
      }
      ret
    }
  }
{{end}}

{{if eq .Cmd ":exec"}}
{{range .Comments}}//{{.}}
{{end}}
  @Throws(SQLException::class)
  {{ if $.EmitInterface }}override {{ end -}}
  override fun {{.MethodName}}({{.Arg.Args}}) {
    conn.prepareStatement({{.ConstantName}}).use { stmt ->
      {{ .Arg.Bindings }}

      stmt.execute()
    }
  }
{{end}}

{{if eq .Cmd ":execrows"}}
{{range .Comments}}//{{.}}
{{end}}
  @Throws(SQLException::class)
  {{ if $.EmitInterface }}override {{ end -}}
  override fun {{.MethodName}}({{.Arg.Args}}): Int {
    return conn.prepareStatement({{.ConstantName}}).use { stmt ->
      {{ .Arg.Bindings }}

      stmt.execute()
      stmt.updateCount
    }
  }
{{end}}

{{if eq .Cmd ":execresult"}}
{{range .Comments}}//{{.}}
{{end}}
  @Throws(SQLException::class)
  {{ if $.EmitInterface }}override {{ end -}}
  override fun {{.MethodName}}({{.Arg.Args}}): Long {
    return conn.prepareStatement({{.ConstantName}}, Statement.RETURN_GENERATED_KEYS).use { stmt ->
      {{ .Arg.Bindings }}

      stmt.execute()

      val results = stmt.generatedKeys
      if (!results.next()) {
          throw SQLException("no generated key returned")
      }
	  results.getLong(1)
    }
  }
{{end}}
{{end}}
}
`

type ktTmplCtx struct {
	Q           string
	Package     string
	Enums       []Enum
	DataClasses []Struct
	Queries     []Query
	Settings    *plugin.Settings
	SqlcVersion string

	// TODO: Race conditions
	SourceName string

	EmitJSONTags        bool
	EmitPreparedQueries bool
	EmitInterface       bool
}

func Offset(v int) int {
	return v + 1
}

func ktFormat(s string) string {
	// TODO: do more than just skip multiple blank lines, like maybe run ktlint to format
	skipNextSpace := false
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		isSpace := len(strings.TrimSpace(l)) == 0
		if !isSpace || !skipNextSpace {
			lines = append(lines, l)
		}
		skipNextSpace = isSpace
	}
	o := strings.Join(lines, "\n")
	o += "\n"
	return o
}

func Generate(ctx context.Context, req *plugin.CodeGenRequest) (*plugin.CodeGenResponse, error) {
	enums := buildEnums(req)
	structs := buildDataClasses(req)
	queries, err := buildQueries(req, structs)
	if err != nil {
		return nil, err
	}

	i := &importer{
		Settings:    req.Settings,
		Enums:       enums,
		DataClasses: structs,
		Queries:     queries,
	}

	funcMap := template.FuncMap{
		"lowerTitle": sdk.LowerTitle,
		"comment":    sdk.DoubleSlashComment,
		"imports":    i.Imports,
		"offset":     Offset,
	}

	modelsFile := template.Must(template.New("table").Funcs(funcMap).Parse(ktModelsTmpl))
	sqlFile := template.Must(template.New("table").Funcs(funcMap).Parse(ktSqlTmpl))
	ifaceFile := template.Must(template.New("table").Funcs(funcMap).Parse(ktIfaceTmpl))

	tctx := ktTmplCtx{
		Settings:    req.Settings,
		Q:           `"""`,
		Package:     req.Settings.Kotlin.Package,
		Queries:     queries,
		Enums:       enums,
		DataClasses: structs,
		SqlcVersion: req.SqlcVersion,
	}

	output := map[string]string{}

	execute := func(name string, t *template.Template) error {
		var b bytes.Buffer
		w := bufio.NewWriter(&b)
		tctx.SourceName = name
		err := t.Execute(w, tctx)
		w.Flush()
		if err != nil {
			return err
		}
		if !strings.HasSuffix(name, ".kt") {
			name += ".kt"
		}
		output[name] = ktFormat(b.String())
		return nil
	}

	if err := execute("Models.kt", modelsFile); err != nil {
		return nil, err
	}
	if err := execute("Queries.kt", ifaceFile); err != nil {
		return nil, err
	}
	if err := execute("QueriesImpl.kt", sqlFile); err != nil {
		return nil, err
	}

	resp := plugin.CodeGenResponse{}

	for filename, code := range output {
		resp.Files = append(resp.Files, &plugin.File{
			Name:     filename,
			Contents: []byte(code),
		})
	}

	return &resp, nil
}
