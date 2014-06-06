package admin

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"github.com/extemporalgenome/slug"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"html/template"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
)

type Admin struct {
	Router        *mux.Router
	Path          string
	Database      string
	Title         string
	NameTransform func(string) string

	Username string
	Password string
	sessions map[string]*session

	SourceDir string

	db            *sql.DB
	models        map[string]*model
	modelGroups   []*modelGroup
	registeredFKs map[reflect.Type]*model
	missingFKs    map[*ForeignKeyField]reflect.Type
}

// Setup registers page handlers and enables the admin.
func Setup(admin *Admin) (*Admin, error) {
	// Source dir / static / templates
	if len(admin.SourceDir) == 0 {
		admin.SourceDir = fmt.Sprintf("%v/src/github.com/oal/admin", os.Getenv("GOPATH"))
	}

	// Load templates (only once, in case we run multiple admins)
	if templates == nil {
		var err error
		templates, err = loadTemplates(fmt.Sprintf("%v/templates", admin.SourceDir))
		if err != nil {
			panic(err)
		}
	}

	// Title
	if len(admin.Title) == 0 {
		admin.Title = "Admin"
	}

	// Users / sessions
	if len(admin.Username) == 0 || len(admin.Password) == 0 {
		return nil, errors.New("Username and/or password is missing")
	}
	admin.sessions = map[string]*session{}

	staticDir := fmt.Sprintf("%v/static/", admin.SourceDir)
	if _, err := os.Stat(staticDir); err != nil {
		return nil, err
	}
	if _, err := os.Stat(fmt.Sprintf("%v/templates/", admin.SourceDir)); err != nil {
		return nil, err
	}

	// Database
	db, err := sql.Open("sqlite3", admin.Database)
	if err != nil {
		return nil, err
	}
	admin.db = db

	// Model init
	admin.models = map[string]*model{}
	admin.modelGroups = []*modelGroup{}
	admin.registeredFKs = map[reflect.Type]*model{}
	admin.missingFKs = map[*ForeignKeyField]reflect.Type{}

	// Routes
	sr := admin.Router.PathPrefix(admin.Path).Subrouter()
	sr.StrictSlash(true)
	sr.HandleFunc("/", admin.handlerWrapper(admin.handleIndex))
	sr.HandleFunc("/logout/", admin.handlerWrapper(admin.handleLogout))
	sr.HandleFunc("/model/{slug}/", admin.handlerWrapper(admin.handleList))
	sr.HandleFunc("/model/{slug}/new/", admin.handlerWrapper(admin.handleEdit))
	sr.HandleFunc("/model/{slug}/{view}/", admin.handlerWrapper(admin.handleList))
	sr.HandleFunc("/model/{slug}/edit/{id}/", admin.handlerWrapper(admin.handleEdit))
	sr.PathPrefix("/static/").Handler(http.StripPrefix("/admin/static/", http.FileServer(http.Dir(staticDir))))

	return admin, nil
}

// Group adds a model group to the admin front page.
// Use this to organize your models.
func (a *Admin) Group(name string) (*modelGroup, error) {
	if a.models == nil {
		return nil, errors.New("Must call .Serve() before adding groups and registering models")
	}

	group := &modelGroup{
		admin:  a,
		Name:   name,
		slug:   slug.SlugAscii(name),
		Models: []*model{},
	}

	a.modelGroups = append(a.modelGroups, group)

	return group, nil
}

type modelGroup struct {
	admin  *Admin
	Name   string
	slug   string
	Models []*model
}

type namedModel interface {
	AdminName() string
}

// RegisterModel adds a model to a model group.
func (g *modelGroup) RegisterModel(mdl interface{}) error {
	modelType := reflect.TypeOf(mdl)

	val := reflect.ValueOf(mdl)
	ind := reflect.Indirect(val)

	parts := strings.Split(modelType.String(), ".")
	name := parts[len(parts)-1]

	var tableName string
	if g.admin.NameTransform != nil {
		tableName = g.admin.NameTransform(name)
	} else {
		tableName = name
	}

	if named, ok := mdl.(namedModel); ok {
		name = named.AdminName()
	}

	am := model{
		Name:      name,
		Slug:      slug.SlugAscii(name),
		tableName: tableName,
		fields:    []Field{},
		instance:  mdl,
	}

	// Set as registered so it can be used as a ForeignKey from other models
	if _, ok := g.admin.registeredFKs[modelType]; !ok {
		g.admin.registeredFKs[modelType] = &am
	}

	// Check if any fields previously registered is missing this model as a foreign key
	for field, modelType := range g.admin.missingFKs {
		if modelType != modelType {
			continue
		}

		field.model = &am
		delete(g.admin.missingFKs, field)
	}

	// Loop over struct fields and set up fields
	for i := 0; i < ind.NumField(); i++ {
		refl := modelType.Elem().Field(i)
		fieldType := refl.Type
		kind := fieldType.Kind()

		// Parse key=val / key options from struct tag, used for configuration later
		tag := refl.Tag.Get("admin")
		if tag == "-" {
			if i == 0 {
				return errors.New("First column (id) can't be skipped.")
			}
			continue
		}
		tagMap, err := parseTag(tag)
		if err != nil {
			panic(err)
		}

		// Expect pointers to be foreignkeys and foreignkeys to have the form Field[Id]
		fieldName := refl.Name
		if kind == reflect.Ptr {
			fieldName += "Id"
		}

		// Transform struct keys to DB column names if needed
		var tableField string
		if g.admin.NameTransform != nil {
			tableField = g.admin.NameTransform(fieldName)
		} else {
			tableField = refl.Name
		}

		// Choose Field
		// First, check if we want to override a field, otherwise use one of the defaults
		var field Field
		overrideField, ok := tagMap["field"]
		if customField, ok2 := customFields[overrideField]; ok && ok2 {
			customType := reflect.ValueOf(customField).Elem().Type()
			newField := reflect.New(customType)
			baseField := newField.Elem().Field(0)
			baseField.Set(reflect.ValueOf(&BaseField{}))
			field = newField.Interface().(Field)
		} else {
			switch kind {
			case reflect.String:
				field = &TextField{BaseField: &BaseField{}}
			case reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				field = &IntField{BaseField: &BaseField{}}
			case reflect.Float32, reflect.Float64:
				field = &FloatField{BaseField: &BaseField{}}
			case reflect.Bool:
				field = &BooleanField{BaseField: &BaseField{}}
			case reflect.Struct:
				field = &TimeField{BaseField: &BaseField{}}
			case reflect.Ptr:
				field = &ForeignKeyField{BaseField: &BaseField{}}

				// Special treatment for foreign keys
				// We need the field to know what model it's related to
				if regModel, ok := g.admin.registeredFKs[fieldType]; ok {
					field.(*ForeignKeyField).model = regModel
				} else {
					g.admin.missingFKs[field.(*ForeignKeyField)] = refl.Type
				}
			default:
				fmt.Println("Unknown field type")
				field = &TextField{BaseField: &BaseField{}}
			}
		}

		field.Attrs().name = fieldName

		// Read relevant config options from the tagMap
		err = field.Configure(tagMap)
		if err != nil {
			panic(err)
		}

		field.Attrs().columnName = tableField
		if label, ok := tagMap["label"]; ok {
			field.Attrs().label = label
		} else {
			field.Attrs().label = fieldName
		}

		if _, ok := tagMap["list"]; ok || i == 0 { // ID (i == 0) is always shown
			field.Attrs().list = true
		}

		if _, ok := tagMap["search"]; ok {
			field.Attrs().searchable = true
		}

		if val, ok := tagMap["default"]; ok {
			field.Attrs().defaultValue = val
		}

		if width, ok := tagMap["width"]; ok {
			i, err := parseInt(width)
			if err != nil {
				panic(err)
			}
			field.Attrs().width = i
		}

		am.fields = append(am.fields, field)
	}

	g.admin.models[am.Slug] = &am
	g.Models = append(g.Models, &am)

	fmt.Println("Registered", am.Name)
	return nil
}

type model struct {
	Name      string
	Slug      string
	fields    []Field
	tableName string
	instance  interface{}
}

func (m *model) renderForm(w io.Writer, data []interface{}, defaults bool, errors []string) {
	hasData := len(data) == len(m.fieldNames())
	var val interface{}
	activeCol := 0
	for i, fieldName := range m.fieldNames() {
		field := m.fieldByName(fieldName)
		if hasData {
			val = data[i]
		} else if defaults {
			val = field.Attrs().defaultValue
		}

		// Error text displayed below field, if any
		var err string
		if errors != nil {
			err = errors[i]
		}

		field.Render(w, val, err, activeCol%12 == 0)
		activeCol += field.Attrs().width
	}
}

func (m *model) fieldNames() []string {
	names := []string{}
	for _, field := range m.fields {
		names = append(names, field.Attrs().name)
	}
	return names
}

func (m *model) tableColumns() []string {
	names := []string{}
	for _, field := range m.fields {
		names = append(names, field.Attrs().columnName)
	}
	return names
}

func (m *model) listColumns() []string {
	names := []string{}
	for _, field := range m.fields {
		if !field.Attrs().list {
			continue
		}
		names = append(names, field.Attrs().label)
	}
	return names
}

func (m *model) listTableColumns() []string {
	names := []string{}
	for _, field := range m.fields {
		if !field.Attrs().list {
			continue
		}
		names = append(names, field.Attrs().columnName)
	}
	return names
}

func (m *model) listFields() []Field {
	fields := []Field{}
	for _, field := range m.fields {
		if !field.Attrs().list {
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

func (m *model) searchableColumns() []string {
	cols := []string{}
	for _, field := range m.fields {
		if !field.Attrs().searchable {
			continue
		}
		cols = append(cols, field.Attrs().columnName)
	}
	return cols
}

func (m *model) fieldByName(name string) Field {
	for _, field := range m.fields {
		if field.Attrs().name == name {
			return field
		}
	}
	return nil
}

func (a *Admin) modelURL(slug, action string) string {
	if _, ok := a.models[slug]; !ok {
		return a.Path
	}

	return fmt.Sprintf("%v/model/%v%v", a.Path, slug, action)
}

func loadTemplates(path string) (*template.Template, error) {
	// Pages / views
	tmpl, err := template.ParseGlob(fmt.Sprintf("%v/*.html", path))
	if err != nil {
		return nil, err
	}

	// Additional functions
	tmpl.Funcs(template.FuncMap{
		"runtemplate": func(name string, ctx interface{}) (template.HTML, error) {
			var buf bytes.Buffer
			err := templates.Lookup(name).Execute(&buf, ctx)
			if err != nil {
				return "", err
			}
			return template.HTML(buf.String()), nil
		},
	})

	tmpl, err = tmpl.ParseGlob(fmt.Sprintf("%v/fields/*.html", path))
	if err != nil {
		return nil, err
	}

	tmpl, err = tmpl.New("FieldWrapper").Parse(`
		{{if .startrow}}</div><div class="row">{{end}}
		<div class="col-sm-{{.width}}">
			<div class="form-group">
				<label for="{{.name}}">{{.label}}</label>
				{{runtemplate .tmpl .}}
				{{if .error}}<p class="text-danger">{{.error}}</p>{{end}}
			</div>
		</div>
	`)
	if err != nil {
		return nil, err
	}

	return tmpl, nil
}
