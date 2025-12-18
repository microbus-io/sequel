/*
Copyright (c) 2025-2026 Microbus LLC and various contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package generator

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/microbus-io/codegen/file"
	fabricgenerator "github.com/microbus-io/codegen/generator"
	"github.com/microbus-io/codegen/sourcecode"
	"github.com/microbus-io/errors"
	"github.com/microbus-io/sequel/codegen/bundle"
	"gopkg.in/yaml.v3"
)

// Generator is the main operator that generates the code.
type Generator struct {
	*fabricgenerator.Generator

	formerObjTypesCached  bool
	formerObjTypeValue    string
	formerObjKeyTypeValue string
	formerTableNameCached bool
	formerTableNameValue  string
}

// NewGenerator creates a new SQL code generator.
// Verbose output is sent to stdout and stderr.
func NewGenerator() (gen *Generator, err error) {
	fabricGen, err := fabricgenerator.NewGenerator()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &Generator{
		Generator: fabricGen,
	}, nil
}

// Run performs code generation.
func (gen *Generator) Run() (err error) {
	// Check if running at the root directory
	if gen.WorkingDir == gen.ProjectDir {
		err = gen.runRoot()
		if err != nil {
			return errors.Trace(err)
		}
	} else {
		err = gen.runService()
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

// runRoot performs code generation inside the root directory.
func (gen *Generator) runRoot() (err error) {
	err = gen.makeClaude()
	if err != nil {
		return errors.Trace(err)
	}
	err = gen.makeConfigLocal()
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// makeClaude populates the .claude directory with files.
func (gen *Generator) makeClaude() (err error) {
	gen.Print("Making .claude")
	gen.Indent()
	defer gen.Unindent()

	err = file.Copy(".claude", bundle.FS, ".claude")
	if err != nil {
		return errors.Trace(err)
	}
	gen.Print(".claude/...")
	return nil
}

// makeConfigLocal creates or updates the config.local.yaml to include the SQLDataSourceName property.
func (gen *Generator) makeConfigLocal() (err error) {
	gen.Print("Making local config")
	gen.Indent()
	defer gen.Unindent()

	exists, err := file.Exists("config.local.yaml")
	if err != nil {
		return errors.Trace(err)
	}
	if !exists {
		err = file.Copy("config.local.yaml", bundle.FS, "config.local.yaml")
		if err != nil {
			return errors.Trace(err)
		}
		gen.Print("config.local.yaml")
		return nil
	}
	scRes, err := sourcecode.ReadFS(bundle.FS, "config.local.yaml")
	if err != nil {
		return errors.Trace(err)
	}
	_, bodyRes := scRes.Block(scRes.MatchFirstLine(`^all:`))

	scDisk, err := sourcecode.ReadFile("config.local.yaml")
	if err != nil {
		return errors.Trace(err)
	}
	scDisk.AppendSpacer()
	insertLine := scDisk.MatchFirstLine(`^all:`)
	if insertLine < 0 {
		scDisk.AppendSpacer()
		scDisk.Append(bodyRes)
		scDisk.AppendSpacer()
	} else {
		_, bodyDisk := scDisk.Block(insertLine)
		for i := 1; i < bodyRes.Len(); i++ {
			prop, _, _ := strings.Cut(bodyRes.Lines[i], ":")
			if bodyDisk.MatchFirstLine(regexp.QuoteMeta(prop)+":") < 0 {
				indentation := scDisk.Indentation(insertLine + 1)
				if indentation == "" {
					indentation = bodyRes.Indentation(i)
				}
				scDisk.InsertAfter(insertLine, indentation+strings.TrimSpace(bodyRes.Lines[i]))
			}
		}
	}
	written, err := scDisk.WriteToFile("config.local.yaml")
	if err != nil {
		return errors.Trace(err)
	}
	if written {
		gen.Print("config.local.yaml")
	}
	return nil
}

// runService performs code generation inside a microservice directory.
func (gen *Generator) runService() (err error) {
	// Init service.yaml
	paused, err := gen.initSpecs()
	if err != nil {
		return errors.Trace(err)
	}
	if paused {
		return nil
	}

	// Add functions and configs to service.yaml
	yamlChanged, err := gen.prepareSpecs()
	if err != nil {
		return errors.Trace(err)
	}

	// Read and parse service.yaml from disk
	content, err := os.ReadFile("service.yaml")
	if err != nil {
		return errors.Trace(err)
	}
	var specs *Service
	err = yaml.Unmarshal(content, &specs)
	if err != nil {
		return errors.Trace(err)
	}
	// Cache current object types and table name
	_, _ = gen.formerObjectTypes()
	_ = gen.formerTableName()

	// Add the CREATE TABLE migration script
	resChanged, err := gen.makeResources(specs)
	if err != nil {
		return errors.Trace(err)
	}

	// Make the public API types
	typesChanged, err := gen.makeTypes(specs)
	if err != nil {
		return errors.Trace(err)
	}

	// Run the Fabric's codegen
	if yamlChanged {
		err = gen.MakeCode()
		if err != nil {
			return errors.Trace(err)
		}
		err = gen.MakeIntegrationTests()
		if err != nil {
			return errors.Trace(err)
		}
		typesChanged = false
		yamlChanged = false
		resChanged = false
	}

	_, err = gen.makeAgentsMD()
	if err != nil {
		return errors.Trace(err)
	}
	implChanged, err := gen.makeImplementation(specs)
	if err != nil {
		return errors.Trace(err)
	}
	testChanged, err := gen.makeIntegrationTest(specs)
	if err != nil {
		return errors.Trace(err)
	}

	// Recalculate the version
	if implChanged || resChanged || typesChanged || yamlChanged || testChanged {
		err = gen.MakeVersion()
		if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}

// initSpecs adds the SQL block to the specs.
func (gen *Generator) initSpecs() (paused bool, err error) {
	gen.Print("Init service.yaml")
	gen.Indent()
	defer gen.Unindent()

	// Be sure service.yaml is up to date
	err = gen.MakeSpecs()
	if err != nil {
		return false, errors.Trace(err)
	}

	scDisk, err := sourcecode.ReadFile("service.yaml")
	if err != nil {
		return false, errors.Trace(err)
	}
	scRes, err := sourcecode.ReadFS(bundle.FS, "service/service.yaml")
	if err != nil {
		return false, errors.Trace(err)
	}

	noun := []rune(gen.PackageName)
	for l := range noun {
		if l == 0 {
			noun[0] = unicode.ToUpper(noun[0])
		} else {
			noun[l] = unicode.ToLower(noun[l])
		}
	}
	sqlCommentRes, sqlBodyRes := scRes.Block(scRes.MatchFirstLine(`^sql\:$`))
	sqlBodyRes.ReplaceAllMatches(`table_name`, strings.ToLower(gen.PackageName))
	sqlBodyRes.ReplaceAllMatches(`Obj`, string(noun))

	sqlDisk := scDisk.MatchFirstLine(`^sql\:$`)
	if sqlDisk < 0 {
		// Add the SQL section
		generalLine := scDisk.MatchFirstLine(`^general\:$`)
		if generalLine < 0 {
			scDisk.AppendSpacer()
			scDisk.Append(sqlCommentRes)
			scDisk.Append(sqlBodyRes)
			scDisk.AppendSpacer()
		} else {
			emptyLine := scDisk.MatchNextLine(generalLine, `^$`)
			if emptyLine < 0 {
				scDisk.AppendSpacer()
				scDisk.Append(sqlCommentRes)
				scDisk.Append(sqlBodyRes)
				scDisk.AppendSpacer()
			} else {
				scDisk.InsertBefore(emptyLine, []string{""})
				scDisk.InsertBefore(emptyLine+1, sqlBodyRes)
				scDisk.InsertBefore(emptyLine+1, sqlCommentRes)
			}
		}
		gen.Print("Added SQL block")
		paused = true
	} else {
		// Refresh the comments
		sqlCommentDisk, _ := scDisk.Block(sqlDisk)
		scDisk.Replace(sqlDisk-sqlCommentDisk.Len(), sqlDisk, sqlCommentRes)
	}

	// Write to disk
	_, err = scDisk.WriteToFile("service.yaml")
	if err != nil {
		return false, errors.Trace(err)
	}
	return paused, nil
}

// prepareSpecs adds configs and functions to the specs.
func (gen *Generator) prepareSpecs() (changed bool, err error) {
	gen.Print("Preparing service.yaml")
	gen.Indent()
	defer gen.Unindent()

	// Read and parse service.yaml from the resource bundle
	content, err := bundle.FS.ReadFile("service/service.yaml")
	if err != nil {
		return false, errors.Trace(err)
	}
	var specsRes *Service
	err = yaml.Unmarshal(content, &specsRes)
	if err != nil {
		return false, errors.Trace(err)
	}

	// Read and parse service.yaml from disk
	content, err = os.ReadFile("service.yaml")
	if err != nil {
		return false, errors.Trace(err)
	}
	var specsDisk *Service
	err = yaml.Unmarshal(content, &specsDisk)
	if err != nil {
		return false, errors.Trace(err)
	}
	scDisk := sourcecode.Parse(string(content))

	// Change the object type in function signatures
	for _, fn := range specsRes.Functions {
		fn.Signature = strings.ReplaceAll(fn.Signature, "Obj", specsDisk.SQL.Object)
	}

	// Add new functions
	insertLine := scDisk.MatchFirstLine(`^functions:$`)
	if insertLine < 0 {
		scDisk.AppendSpacer()
		scDisk.Append("functions:")
		insertLine = scDisk.Len() - 1
	}
	existing := map[string]bool{}
	for _, fn := range specsDisk.Functions {
		existing[fn.Name()] = true
	}
	for _, fn := range specsRes.Functions {
		if existing[fn.Name()] {
			continue
		}
		scDisk.InsertAfter(insertLine, fmt.Sprintf("  - signature: %s\n    description: %s", fn.Signature, fn.Description))
		insertLine += 2
		gen.Print("Added function %s", fn.Name())
	}

	// Update signatures of functions
	sigsByName := map[string]string{}
	for _, fn := range specsRes.Functions {
		sigsByName[fn.Name()] = fn.Signature
	}
	for _, i := range scDisk.MatchAllLines(`^  - signature:`) {
		name := strings.TrimPrefix(scDisk.Lines[i], "  - signature:")
		name = strings.TrimSpace(name)
		name, _, _ = strings.Cut(name, "(")
		name = strings.TrimSpace(name)
		if sigsByName[name] != "" && scDisk.Lines[i] != "  - signature: "+sigsByName[name] {
			scDisk.Lines[i] = "  - signature: " + sigsByName[name]
			gen.Print("Updated function %s", name)
		}
	}

	// Add new configs
	insertLine = scDisk.MatchFirstLine(`^configs:$`)
	if insertLine < 0 {
		scDisk.AppendSpacer()
		scDisk.Append("configs:")
		insertLine = scDisk.Len() - 1
	}
	existing = map[string]bool{}
	for _, fn := range specsDisk.Configs {
		existing[fn.Name()] = true
	}
	for _, fn := range specsRes.Configs {
		if existing[fn.Name()] {
			continue
		}
		scDisk.InsertAfter(insertLine, fmt.Sprintf("  - signature: %s\n    description: %s", fn.Signature, fn.Description))
		insertLine += 2
		gen.Print("Added config %s", fn.Name())
	}

	// Update signatures of configs
	sigsByName = map[string]string{}
	for _, fn := range specsRes.Configs {
		sigsByName[fn.Name()] = fn.Signature
	}
	for _, i := range scDisk.MatchAllLines(`^  - signature:`) {
		name := strings.TrimPrefix(scDisk.Lines[i], "  - signature:")
		name = strings.TrimSpace(name)
		name, _, _ = strings.Cut(name, "(")
		name = strings.TrimSpace(name)
		if sigsByName[name] != "" && scDisk.Lines[i] != "  - signature: "+sigsByName[name] {
			scDisk.Lines[i] = "  - signature: " + sigsByName[name]
			gen.Print("Updated config %s", name)
		}
	}

	// Write to disk
	written, err := scDisk.WriteToFile("service.yaml")
	if err != nil {
		return false, errors.Trace(err)
	}
	return written, nil
}

// makeTypes creates the types in the API directory of the microservice for the object, key and query.
func (gen *Generator) makeTypes(specs *Service) (changed bool, err error) {
	gen.Print("Making types")
	gen.Indent()
	defer gen.Unindent()

	// Mkdir serviceapi (should already exist)
	dir := filepath.Base(gen.WorkingDir) + "api"
	created, err := file.Mkdir(dir)
	if err != nil {
		return false, errors.Trace(err)
	}
	if created {
		gen.Print("mkdir %s", dir)
	}

	formerObjType, formerObjKeyType := gen.formerObjectTypes()
	renameObjectTypes := func(fileName string) error {
		scDisk, err := sourcecode.ReadFile(fileName)
		if err != nil {
			return errors.Trace(err)
		}
		if formerObjKeyType != "" && formerObjKeyType != specs.SQL.Object+"Key" {
			scDisk.ReplaceAllMatches(`(^|\b)`+regexp.QuoteMeta(formerObjKeyType)+`($|\b)`, specs.SQL.Object+"Key")
			if strings.HasSuffix(fileName, "objectkey.go") {
				line := scDisk.MatchFirstLine(`^` + regexp.QuoteMeta("type "+specs.SQL.Object+"Key struct {"))
				if line >= 0 {
					comment, _ := scDisk.Block(line)
					scDisk.InsertBefore(line-comment.Len(), []string{
						"type " + formerObjKeyType + " = " + specs.SQL.Object + "Key",
						"",
					})
				}
			}
		}
		if formerObjType != "" && formerObjType != specs.SQL.Object {
			scDisk.ReplaceAllMatches(`(^|\b)`+regexp.QuoteMeta(formerObjType)+`($|\b)`, specs.SQL.Object)
			if strings.HasSuffix(fileName, "object.go") {
				line := scDisk.MatchFirstLine(`^` + regexp.QuoteMeta("type "+specs.SQL.Object+" struct {"))
				if line >= 0 {
					comment, _ := scDisk.Block(line)
					scDisk.InsertBefore(line-comment.Len(), []string{
						"type " + formerObjType + " = " + specs.SQL.Object,
						"",
					})
				}
			}
		}
		written, err := scDisk.WriteToFile(fileName)
		if err != nil {
			return errors.Trace(err)
		}
		if written {
			gen.Print("%s", fileName)
			changed = true
		}
		return nil
	}

	// serviceapi/objectkey.go
	fileName := filepath.Join(dir, "objectkey.go")
	exists, err := file.Exists(fileName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, errors.Trace(err)
	}
	if !exists {
		scRes, err := sourcecode.ReadFS(bundle.FS, "service/serviceapi/objectkey.go")
		if err != nil {
			return false, errors.Trace(err)
		}
		keyBuf := make([]byte, 16)
		nonceBuf := make([]byte, 16)
		rand.Read(keyBuf)
		rand.Read(nonceBuf)
		scRes.ReplaceAllMatches(`_KEY_`, hex.EncodeToString(keyBuf))
		scRes.ReplaceAllMatches(`_NONCE_`, hex.EncodeToString(nonceBuf))
		scRes.ReplaceAllMatches(`serviceapi`, filepath.Base(gen.PackageName)+"api")
		scRes.ReplaceAllMatches(`ObjKey`, specs.SQL.Object+"Key")
		_, err = scRes.WriteToFile(fileName)
		if err != nil {
			return false, errors.Trace(err)
		}
		gen.Print("%s", fileName)
		changed = true
	} else {
		err = renameObjectTypes(fileName)
		if err != nil {
			return false, errors.Trace(err)
		}
	}

	// serviceapi/object.go
	fileName = filepath.Join(dir, "object.go")
	exists, err = file.Exists(fileName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, errors.Trace(err)
	}
	if !exists {
		sc, err := sourcecode.ReadFS(bundle.FS, "service/serviceapi/object.go")
		if err != nil {
			return false, errors.Trace(err)
		}
		sc.ReplaceAllMatches(`serviceapi`, filepath.Base(gen.PackageName)+"api")
		sc.ReplaceAllMatches(`Obj`, specs.SQL.Object)
		_, err = sc.WriteToFile(fileName)
		if err != nil {
			return false, errors.Trace(err)
		}
		gen.Print("%s", fileName)
		changed = true
	} else {
		err = renameObjectTypes(fileName)
		if err != nil {
			return false, errors.Trace(err)
		}
	}

	// serviceapi/query.go
	fileName = filepath.Join(dir, "query.go")
	exists, err = file.Exists(fileName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, errors.Trace(err)
	}
	if !exists {
		sc, err := sourcecode.ReadFS(bundle.FS, "service/serviceapi/query.go")
		if err != nil {
			return false, errors.Trace(err)
		}
		sc.ReplaceAllMatches(`serviceapi`, filepath.Base(gen.PackageName)+"api")
		sc.ReplaceAllMatches(`ObjKey`, specs.SQL.Object+"Key")
		_, err = sc.WriteToFile(fileName)
		if err != nil {
			return false, errors.Trace(err)
		}
		gen.Print("%s", fileName)
		changed = true
	} else {
		err = renameObjectTypes(fileName)
		if err != nil {
			return false, errors.Trace(err)
		}
	}

	// serviceapi/query_test.go
	fileName = filepath.Join(dir, "query_test.go")
	exists, err = file.Exists(fileName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, errors.Trace(err)
	}
	if !exists {
		sc, err := sourcecode.ReadFS(bundle.FS, "service/serviceapi/query_test.go")
		if err != nil {
			return false, errors.Trace(err)
		}
		sc.ReplaceAllMatches(`serviceapi`, filepath.Base(gen.PackageName)+"api")
		sc.ReplaceAllMatches(`ObjKey`, specs.SQL.Object+"Key")
		sc.ReplaceAllMatches(`Obj([^a-zA-Z])`, specs.SQL.Object+"${1}")
		_, err = sc.WriteToFile(fileName)
		if err != nil {
			return false, errors.Trace(err)
		}
		gen.Print("%s", fileName)
		changed = true
	} else {
		err = renameObjectTypes(fileName)
		if err != nil {
			return false, errors.Trace(err)
		}
	}

	return changed, nil
}

// makeAgentsMD updates the local AGENTS.md file of the microservice.
func (gen *Generator) makeAgentsMD() (changed bool, err error) {
	gen.Print("Making agent instructions")
	gen.Indent()
	defer gen.Unindent()

	// Create or update AGENTS.md
	for _, filename := range []string{"AGENTS.md"} {
		scRes, err := sourcecode.ReadFS(bundle.FS, "service/"+filename)
		if err != nil {
			return false, errors.Trace(err)
		}
		scDisk, err := sourcecode.ReadFile(filename)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, errors.Trace(err)
		}
		if scDisk == nil {
			changed, err = scRes.WriteToFile(filename)
			if err != nil {
				return false, errors.Trace(err)
			}
			gen.Print(filename)
		} else {
			if !strings.Contains(scDisk.Content(), scRes.Content()) {
				scDisk.InsertBefore(0, scRes)
				changed, err = scDisk.WriteToFile(filename)
				if err != nil {
					return false, errors.Trace(err)
				}
				gen.Print(filename)
			}
		}
	}
	return changed, nil
}

// makeImplementation inserts the implementation to newly created functions in service.go
func (gen *Generator) makeImplementation(specs *Service) (changed bool, err error) {
	gen.Print("Making implementation")
	gen.Indent()
	defer gen.Unindent()

	// Adjust service.go
	scRes, err := sourcecode.ReadFS(bundle.FS, "service/service.go")
	if err != nil {
		return false, errors.Trace(err)
	}
	scRes.ReplaceAllMatches(`serviceapi\.Obj`, gen.PackageName+"api."+specs.SQL.Object)
	scRes.ReplaceAllMatches(`serviceapi\.`, gen.PackageName+"api.")
	scRes.ReplaceAllMatches("table_name", specs.SQL.Table)
	scRes.ReplaceAllMatches("_SEQUENCE_", fmt.Sprintf("%s@%08x", specs.SQL.Table, rand.Int31()))

	scDisk, err := sourcecode.ReadFile("service.go")
	if err != nil {
		return false, errors.Trace(err)
	}
	formerObjType, formerObjKeyType := gen.formerObjectTypes()
	if formerObjKeyType != "" && formerObjKeyType != specs.SQL.Object+"Key" {
		scDisk.ReplaceAllMatches(regexp.QuoteMeta("api."+formerObjKeyType)+`($|\b)`, "api."+specs.SQL.Object+"Key")
	}
	if formerObjType != "" && formerObjType != specs.SQL.Object {
		scDisk.ReplaceAllMatches(regexp.QuoteMeta("api."+formerObjType)+`($|\b)`, "api."+specs.SQL.Object)
	}
	formerTableName := gen.formerTableName()
	if formerTableName != "" && formerTableName != specs.SQL.Table {
		scDisk.ReplaceAllMatches(`tableName = "[^"]+"`, `tableName = "`+specs.SQL.Table+`"`)
	}

	// Add lowercase functions to service.go
	for _, i := range scRes.MatchAllLines(`^` + regexp.QuoteMeta("func (svc *Service) ") + `[a-z]`) {
		searchStr, _, _ := strings.Cut(scRes.Lines[i], "(ctx")
		if scDisk.MatchFirstLine(`^`+regexp.QuoteMeta(searchStr)) < 0 {
			comment, body := scRes.Block(i)
			scDisk.AppendSpacer()
			scDisk.Append(comment)
			scDisk.Append(body)
			scDisk.AppendSpacer()
		}
	}

	// Add lifecycle functions
	for _, i := range scRes.MatchAllLines(`^` + regexp.QuoteMeta("func (svc *Service) On")) {
		searchStr, _, _ := strings.Cut(scRes.Lines[i], "(ctx")
		baseLine := scDisk.MatchFirstLine(`^` + regexp.QuoteMeta(searchStr))
		if baseLine < 0 {
			comment, body := scRes.Block(i)
			scDisk.AppendSpacer()
			scDisk.Append(comment)
			scDisk.Append(body)
			scDisk.AppendSpacer()
		} else {
			endLine := scDisk.MatchNextLine(baseLine, `^\}$`)
			next := scDisk.MatchNextLine(baseLine, regexp.QuoteMeta("svc.")+`(open|close)`+regexp.QuoteMeta("Database(ctx)"))
			if next < 0 || next > endLine {
				_, body := scRes.Block(i)
				body = body.Sub(1, body.Len()-2) // Omit final return
				scDisk.InsertAfter(baseLine, body)
			}
		}
	}

	// Add handlers functions
	for _, i := range scRes.MatchAllLines(`^` + regexp.QuoteMeta("func (svc *Service) ") + `[A-Z][a-zA-Z]*\(ctx`) {
		funcNameSig, _, _ := strings.Cut(scRes.Lines[i], "(ctx")
		if strings.Contains(funcNameSig, "OnStartup") || strings.Contains(funcNameSig, "OnShutdown") {
			continue
		}
		baseLine := scDisk.MatchFirstLine(`^` + regexp.QuoteMeta(funcNameSig))
		if baseLine < 0 {
			comment, body := scRes.Block(i)
			scDisk.AppendSpacer()
			scDisk.Append(comment)
			scDisk.Append(body)
			scDisk.AppendSpacer()
		} else {
			endLine := scDisk.MatchNextLine(baseLine, `^\}$`)
			next := scDisk.MatchNextLine(baseLine, regexp.QuoteMeta("// TO"+"DO: Implement"))
			if next > baseLine && next < endLine {
				_, body := scRes.Block(i)
				scDisk.Replace(baseLine, endLine+1, body)
			}
		}
	}

	// Add imports
	baseLine := scRes.MatchFirstLine(`^import \($`)
	if baseLine >= 0 {
		for i := baseLine + 1; i < scRes.Len(); i++ {
			stmt := strings.TrimSpace(scRes.Lines[i])
			if stmt == ")" {
				break
			}
			if strings.Contains(stmt, "/connector") {
				scDisk.AddImport(`"github.com/microbus-io/fabric/connector"`)
			} else if strings.Contains(stmt, "/frame") {
				scDisk.AddImport(`"github.com/microbus-io/fabric/frame"`)
			} else if stmt != "" && !strings.Contains(stmt, "/service/serviceapi") {
				scDisk.AddImport(stmt)
			}
		}
	}

	// Add consts
	baseLine = scRes.MatchFirstLine(`^const \($`)
	if baseLine >= 0 {
		for i := baseLine + 1; i < scRes.Len(); i++ {
			stmt := strings.TrimSpace(scRes.Lines[i])
			if stmt == ")" {
				break
			}
			if stmt != "" {
				scDisk.AddConst(stmt)
			}
		}
	}

	// Add member variable
	serviceMemberVars := []string{}
	baseLine = scRes.MatchFirstLine(`^type Service struct \{$`)
	if baseLine >= 0 {
		for i := baseLine + 1; i < scRes.Len(); i++ {
			def := strings.TrimSpace(scRes.Lines[i])
			if def == "}" {
				break
			}
			if def != "" {
				serviceMemberVars = append(serviceMemberVars, def)
			}
		}
	}
	baseLine = scDisk.MatchFirstLine(`^type Service struct \{$`)
	if baseLine >= 0 {
		endLine := scDisk.MatchNextLine(baseLine, `^\}$`)
		if endLine >= 0 {
			for _, def := range serviceMemberVars {
				line := scDisk.MatchNextLine(baseLine, `^`+regexp.QuoteMeta("\t"+def)+`$`)
				if line < 0 || line > endLine {
					scDisk.InsertBefore(endLine, "\t"+def)
				}
			}
		}
	}

	// Write to disk
	written, err := scDisk.WriteToFile("service.go")
	if err != nil {
		return false, errors.Trace(err)
	}
	if written {
		changed = true
		gen.Print("service.go")
	}
	return changed, nil
}

// makeIntegrationTest inserts test cases to newly created functions in service_test.go
func (gen *Generator) makeIntegrationTest(specs *Service) (changed bool, err error) {
	gen.Print("Making integration tests")
	gen.Indent()
	defer gen.Unindent()

	scRes, err := sourcecode.ReadFS(bundle.FS, "service/service_test.go")
	if err != nil {
		return false, errors.Trace(err)
	}
	scRes.ReplaceAllMatches(`serviceapi\.Obj`, gen.PackageName+"api."+specs.SQL.Object)
	scRes.ReplaceAllMatches(`serviceapi\.`, gen.PackageName+"api.")

	scDisk, err := sourcecode.ReadFile("service_test.go")
	if errors.Is(err, os.ErrNotExist) {
		gen.Print("service_test.go not found")
		return false, nil
	}
	if err != nil {
		return false, errors.Trace(err)
	}
	formerObjType, formerObjKeyType := gen.formerObjectTypes()
	if formerObjKeyType != "" && formerObjKeyType != specs.SQL.Object+"Key" {
		scDisk.ReplaceAllMatches(regexp.QuoteMeta("api."+formerObjKeyType)+`($|\b)`, "api."+specs.SQL.Object+"Key")
	}
	if formerObjType != "" && formerObjType != specs.SQL.Object {
		scDisk.ReplaceAllMatches(regexp.QuoteMeta("api."+formerObjType)+`($|\b)`, "api."+specs.SQL.Object)
	}

	// Add test cases
	for _, i := range scRes.MatchAllLines(`^func Test[^_]+_[^\(]+\(t \*testing\.T\) \{`) {
		fnName := regexp.MustCompile(`func Test[^_]+_([^\(]+)\(`).FindStringSubmatch(scRes.Lines[i])[1]

		srcStart := scRes.MatchNextLine(i, `^\tt\.Run\(`)
		if srcStart < 0 {
			continue
		}
		srcEnd := scRes.MatchNextLine(i, `^\}$`)
		if srcEnd < 0 {
			continue
		}

		baseLine := scDisk.MatchFirstLine(`^func Test[^_]+_` + regexp.QuoteMeta(fnName) + `\(t \*testing\.T\) \{`)
		if baseLine < 0 {
			continue
		}
		todoLine := scDisk.MatchNextLine(baseLine, regexp.QuoteMeta("// TO"+"DO: Test"))
		if todoLine < 0 {
			continue
		}
		endLine := scDisk.MatchNextLine(baseLine, `^\}$`)
		if endLine < 0 || todoLine > endLine {
			continue
		}
		scDisk.Replace(todoLine, todoLine+1, scRes.Sub(srcStart, srcEnd))
	}

	// math/rand is used in one of the tests
	scDisk.AddImport(`"math/rand"`)
	scDisk.AddImport(`"strconv"`)
	scDisk.AddImport(`"strings"`)
	scDisk.AddImport(`"sync"`)
	scDisk.AddImport(`"sort"`)
	scDisk.AddImport(`"time"`)
	scDisk.AddVar("_ rand.Source")
	scDisk.AddVar("_ strconv.NumError")
	scDisk.AddVar("_ *strings.Builder")
	scDisk.AddVar("_ *sync.WaitGroup")
	scDisk.AddVar("_ sort.Interface")
	scDisk.AddVar("_ time.Time")

	testNamePrefix := ""
	line := scDisk.MatchFirstLine(`^func Test[^_]+_[^\(]+\(t \*testing\.T\) \{`)
	if line >= 0 {
		testNamePrefix = regexp.MustCompile(`func (Test[^_]+)_`).FindStringSubmatch(scDisk.Lines[line])[1]
	}

	for _, fnName := range []string{"NewObject", "TestService_ValidateObject"} {
		regExpStr := `^` + regexp.QuoteMeta("func "+fnName+"(")
		baseLine := scRes.MatchFirstLine(regExpStr)
		if baseLine >= 0 {
			if scDisk.MatchFirstLine(regExpStr) < 0 {
				_, resBody := scRes.Block(baseLine)
				if testNamePrefix != "" {
					resBody.ReplaceAllMatches(`func TestService`, "func "+testNamePrefix)
				}
				scDisk.AppendSpacer()
				scDisk.Append(resBody)
				scDisk.AppendSpacer()
			}
		}
	}

	written, err := scDisk.WriteToFile("service_test.go")
	if err != nil {
		return false, errors.Trace(err)
	}
	return written, nil
}

func (gen *Generator) makeResources(specs *Service) (changed bool, err error) {
	gen.Print("Making resources")
	gen.Indent()
	defer gen.Unindent()

	// Mkdir
	dirCreated, err := file.Mkdir("resources/sql")
	if err != nil {
		return false, errors.Trace(err)
	}
	if dirCreated {
		gen.Print("mkdir resources/sql")
		changed = true
	}

	// Script to create the table
	exists, err := file.Exists("resources/sql/1.sql")
	if err != nil {
		return false, errors.Trace(err)
	}
	if !exists {
		sc, err := sourcecode.ReadFS(bundle.FS, "service/resources/sql/1.sql")
		if err != nil {
			return false, errors.Trace(err)
		}
		sc.ReplaceAllMatches("table_name", specs.SQL.Table)
		_, err = sc.WriteToFile("resources/sql/1.sql")
		if err != nil {
			return false, errors.Trace(err)
		}
		gen.Print("resources/sql/1.sql")
		changed = true
	}

	// Script to rename the table
	formerTableName := gen.formerTableName()
	if formerTableName != "" && formerTableName != specs.SQL.Table {
		// Find latest SQL script file name
		entries, err := os.ReadDir("resources/sql")
		if err != nil {
			return false, errors.Trace(err)
		}
		maxNum := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sql") {
				s, _, _ := strings.Cut(e.Name(), ".")
				n, _ := strconv.Atoi(s)
				maxNum = max(maxNum, n)
			}
		}
		maxNum++
		// Add the script
		sc, err := sourcecode.ReadFS(bundle.FS, "service/resources/sql/renametable.sql")
		if err != nil {
			return false, errors.Trace(err)
		}
		sc.ReplaceAllMatches("old_table_name", formerTableName)
		sc.ReplaceAllMatches("new_table_name", specs.SQL.Table)
		_, err = sc.WriteToFile("resources/sql/" + strconv.Itoa(maxNum) + ".sql")
		if err != nil {
			return false, errors.Trace(err)
		}
		gen.Print("resources/sql/" + strconv.Itoa(maxNum) + ".sql")
		changed = true
	}

	return changed, nil
}

// formerObjectTypes returns the former object and object key types, if present in the code.
func (gen *Generator) formerObjectTypes() (objType string, objKeyType string) {
	if gen.formerObjTypesCached {
		return gen.formerObjTypeValue, gen.formerObjKeyTypeValue
	}
	fileName := path.Join(filepath.Base(gen.WorkingDir)+"api", "object.go")
	sc, err := sourcecode.ReadFile(fileName)
	if err == nil {
		line := sc.MatchFirstLine(`^type [A-Z][a-zA-Z0-9]* struct \{$`)
		if line >= 0 {
			objType = sc.Lines[line]
			_, objType, _ = strings.Cut(objType, " ")
			objType, _, _ = strings.Cut(objType, " ")
			objType = strings.TrimSpace(objType)
		}
	}
	fileName = path.Join(filepath.Base(gen.WorkingDir)+"api", "objectkey.go")
	sc, err = sourcecode.ReadFile(fileName)
	if err == nil {
		line := sc.MatchFirstLine(`^type [A-Z][a-zA-Z0-9]*Key struct \{$`)
		if line >= 0 {
			objKeyType = sc.Lines[line]
			_, objKeyType, _ = strings.Cut(objKeyType, " ")
			objKeyType, _, _ = strings.Cut(objKeyType, " ")
			objKeyType = strings.TrimSpace(objKeyType)
		}
	}
	gen.formerObjTypesCached = true
	gen.formerObjTypeValue = objType
	gen.formerObjKeyTypeValue = objKeyType
	return objType, objKeyType
}

// formerTableName returns the former name of the table, if present in the code.
func (gen *Generator) formerTableName() (tableName string) {
	if gen.formerTableNameCached {
		return gen.formerTableNameValue
	}
	fileName := path.Join("service.go")
	sc, err := sourcecode.ReadFile(fileName)
	if err == nil {
		line := sc.MatchFirstLine(`tableName = "[^"]+"`)
		if line >= 0 {
			tableName = regexp.MustCompile(`"([^"]+)"`).FindStringSubmatch(sc.Lines[line])[1]
		}
	}
	gen.formerTableNameCached = true
	gen.formerTableNameValue = tableName
	return tableName
}
