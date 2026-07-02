// tetogen - Turn go.lang structs into .yaml config files.
// Copyright (C) 2026  yu-lab31
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	projHome := findProjectHome()
	if err := os.Chdir(projHome); err != nil {
		log.Fatalf("Cannot cd to home of this project: %v\n", err)
	}

	fmt.Println("Start scanning for target go structs...")

	fset := token.NewFileSet()

	err := filepath.WalkDir(projHome,
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if !strings.HasSuffix(path, ".go") {
				return nil
			}

			node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				fmt.Printf("Failed to parse (%v): %s\n", err, path)
				return nil
			}

			ast.Inspect(node, func(n ast.Node) bool {
				genDecl, ok := n.(*ast.GenDecl)
				if !ok || genDecl.Tok != token.TYPE || genDecl.Doc == nil {
					return true
				}

				var magicComment string
				for _, comment := range genDecl.Doc.List {
					if strings.HasPrefix(comment.Text, "// +tetogen:") {
						magicComment = comment.Text
						break
					}
				}

				if magicComment == "" {
					return true
				}

				params := parseMagicComment(magicComment)

				for _, spec := range genDecl.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}

					if structType, ok := typeSpec.Type.(*ast.StructType); ok {
						structName := strings.ToLower(typeSpec.Name.Name)
						generate(structName, structType, params)
					}
				}

				return true
			})

			return nil
		})

	if err != nil {
		log.Fatalf("Failed to scan: %v", err)
	}
}

func generate(structName string, st *ast.StructType, params map[string]string) {
	outPath := params["out"]
	if outPath == "" {
		outPath = findProjectHome()
	}

	profilesRaw := params["profiles"]
	profiles := []string{""}
	if profilesRaw != "" {
		profiles = strings.Split(profilesRaw, ",")
	}

	yamlData, schemaProps := parseAst(st)

	schemaRoot := map[string]any{
		"$schema":    "http://json-schema.org/draft-07/schema#",
		"type":       "object",
		"properties": schemaProps,
	}

	os.MkdirAll(outPath, 0755)

	schemaFileName := fmt.Sprintf("%s.schema.json", structName)
	schemaPath := filepath.Join(outPath, schemaFileName)
	schemaBytes, _ := json.MarshalIndent(schemaRoot, "", "  ")
	os.WriteFile(schemaPath, schemaBytes, 0644)

	for _, profile := range profiles {
		yamlBytes, _ := yaml.Marshal(yamlData)

		header := fmt.Sprintf("# yaml-language-server: $schema=./%s\n",
			schemaFileName)
		content := header + string(yamlBytes)

		var yamlFileName string
		if profilesRaw == "" {
			yamlFileName = structName + ".yaml"
		} else {
			yamlFileName = fmt.Sprintf("%s_%s.yaml", structName, profile)
		}
		yamlPath := filepath.Join(outPath, yamlFileName)
		os.WriteFile(yamlPath, []byte(content), 0644)
		fmt.Printf("Generated: %s\n", yamlPath)
	}
}

func parseAst(st *ast.StructType) (map[string]any, map[string]any) {
	yamlResult := make(map[string]any)
	schemaResult := make(map[string]any)

	for _, field := range st.Fields.List {
		var yamlKey, envDefault string
		if field.Tag != nil {
			tags, _ := strconv.Unquote(field.Tag.Value)
			strucTag := reflect.StructTag(tags)

			yamlKey = strucTag.Get("yaml")
			envDefault = strucTag.Get("env-default")
		}

		if yamlKey == "" || yamlKey == "-" {
			if len(field.Names) > 0 {
				yamlKey = field.Names[0].Name
			} else {
				continue
			}
		}

		switch fieldType := field.Type.(type) {
		case *ast.StructType:
			{
				subYaml, subSchemaProps := parseAst(fieldType)
				yamlResult[yamlKey] = subYaml
				schemaResult[yamlKey] = map[string]any{
					"type":       "object",
					"properties": subSchemaProps,
				}
			}
		case *ast.Ident:
			{
				yamlResult[yamlKey] = formatYamlValue(envDefault, fieldType.Name)
				schemaResult[yamlKey] = map[string]any{
					"type": getJsonSchemaType(fieldType.Name),
				}
			}
		case *ast.ArrayType:
			{
				if envDefault != "" {
					yamlResult[yamlKey] = strings.Split(envDefault, ",")
				} else {
					yamlResult[yamlKey] = []any{}
				}

				itemType := "string"
				if ident, ok := fieldType.Elt.(*ast.Ident); ok {
					itemType = getJsonSchemaType(ident.Name)
				}
				schemaResult[yamlKey] = map[string]any{
					"type":  "array",
					"items": map[string]any{"type": itemType},
				}
			}
		case *ast.MapType:
			{
				yamlResult[yamlKey] = map[string]any{}

				valType := "string"
				if ident, ok := fieldType.Value.(*ast.Ident); ok {
					valType = getJsonSchemaType(ident.Name)
				}
				schemaResult[yamlKey] = map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": valType},
				}
			}
		}
	}

	return yamlResult, schemaResult
}

func formatYamlValue(val string, typeName string) any {
	if val == "" {
		return nil
	}
	switch typeName {
	case "int", "int64", "int32":
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	case "bool":
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return val
}

func getJsonSchemaType(goType string) string {
	switch goType {
	case "int", "int64", "int32", "float64", "float32":
		return "integer"
	case "bool":
		return "boolean"
	default:
		return "string"
	}
}

func parseMagicComment(comment string) map[string]string {
	params := make(map[string]string)
	comment = strings.TrimPrefix(comment, "// +tetogen:")
	fields := strings.FieldsSeq(comment)
	for field := range fields {
		pair := strings.SplitN(field, "=", 2)
		if len(pair) == 2 {
			params[pair[0]] = pair[1]
		}
	}
	return params
}

func findProjectHome() string {
	curDir, err := os.Getwd()
	if err != nil {
		return "."
	}

	for {
		if _, err := os.Stat(filepath.Join(curDir, "go.mod")); err == nil {
			return curDir
		}
		parentDir := filepath.Dir(curDir)
		if parentDir == curDir {
			break
		}
		curDir = parentDir
	}
	return "."
}
