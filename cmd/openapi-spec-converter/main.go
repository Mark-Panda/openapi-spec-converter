package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	openapispecconverter "github.com/dense-analysis/openapi-spec-converter"
	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	ghodssYaml "github.com/ghodss/yaml"
	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/datamodel/low"
	"github.com/pb33f/libopenapi/utils"
	"github.com/pborman/getopt/v2"
	"gopkg.in/yaml.v3"
)

// SpecVersion 表示 OpenAPI 规范版本类型
type SpecVersion int

const (
	Swagger   SpecVersion = iota // Swagger 2.0
	OpenAPI30                    // OpenAPI 3.0
	OpenAPI31                    // OpenAPI 3.1
)

// Format 表示输出格式类型
type Format int

const (
	JSON Format = iota // JSON 格式
	YAML               // YAML 格式
)

// Arguments 存储命令行参数解析后的结果
type Arguments struct {
	inputFilename  string      // 输入文件名（"-" 表示从标准输入读取）
	outputFilename string      // 输出文件名（空字符串表示输出到标准输出）
	outputTarget   SpecVersion // 目标版本（Swagger/OpenAPI30/OpenAPI31）
	outputFormat   Format      // 输出格式（JSON/YAML）
}

// parseArgs 解析命令行参数并返回 Arguments 结构体。
// 支持的参数：
//   - --help, -h: 显示帮助信息
//   - --output, -o: 指定输出文件（默认为标准输出）
//   - --target, -t: 指定目标版本，可选值：swagger, 3.0, 3.1（默认为 3.1）
//   - --format, -f: 指定输出格式，可选值：json, yaml（默认为 json）
//   - <input>: 输入文件名（可选，如果不提供则从标准输入读取）
//
// 返回：解析后的 Arguments 结构体
func parseArgs() Arguments {
	var arguments Arguments

	getopt.SetProgram(filepath.Base(os.Args[0]))

	showHelp := getopt.BoolLong("help", 'h', "Print this help message")
	outputFilename := getopt.StringLong("output", 'o', "", "Output file (default stdout)")
	outputVersion := getopt.StringLong("target", 't', "3.1", "Target version: swagger, 3.0, or 3.1")
	outputFormat := getopt.StringLong("format", 'f', "json", "Output format: yaml or json")
	getopt.SetParameters("<input>")

	getopt.Parse()

	if showHelp != nil && *showHelp {
		getopt.PrintUsage(os.Stdout)
		os.Exit(0)
	}

	args := getopt.Args()

	if len(args) > 2 {
		fmt.Fprintln(os.Stderr, "Invalid number of arguments")
		getopt.PrintUsage(os.Stderr)
		os.Exit(1)
	}

	if len(args) == 0 {
		// If no arguments are supplied and there's no data being piped in,
		// then complain and print usage.
		if stat, err := os.Stdin.Stat(); err != nil || (stat.Mode()&os.ModeCharDevice) != 0 {
			fmt.Fprintln(os.Stderr, "No input filename or open stdin pipe")
			getopt.PrintUsage(os.Stderr)
			os.Exit(1)
		}

		arguments.inputFilename = "-"
	} else {
		arguments.inputFilename = args[0]
	}

	if len(arguments.inputFilename) == 0 {
		fmt.Fprintln(os.Stderr, "Empty input filename")
		getopt.PrintUsage(os.Stderr)
		os.Exit(1)
	}

	arguments.outputFilename = *outputFilename

	switch strings.ToLower(*outputVersion) {
	case "swagger":
		arguments.outputTarget = Swagger
	case "3.0":
		arguments.outputTarget = OpenAPI30
	case "3.1":
		arguments.outputTarget = OpenAPI31
	default:
		fmt.Fprintf(os.Stderr, "Invalid target version %s\n", *outputVersion)
		getopt.PrintUsage(os.Stderr)
		os.Exit(1)
	}

	switch strings.ToLower(*outputFormat) {
	case "json":
		arguments.outputFormat = JSON
	case "yaml":
		arguments.outputFormat = YAML
	default:
		fmt.Fprintf(os.Stderr, "Invalid format: %s\n", *outputFormat)
		getopt.PrintUsage(os.Stderr)
		os.Exit(1)
	}

	return arguments
}

// readInputFile 根据参数读取输入文件内容。
// 输入源：
//   - 如果 arguments.inputFilename == "-"，则从标准输入（os.Stdin）读取
//   - 否则从指定文件路径读取
//
// 返回：文件内容的字节数组和可能的错误
func readInputFile(arguments Arguments) (inputData []byte, err error) {
	if arguments.inputFilename == "-" {
		inputData, err = io.ReadAll(os.Stdin)
	} else {
		inputData, err = os.ReadFile(arguments.inputFilename)
	}

	return
}

// make30RequiredAndReadonlyPropertiesOnlyReadonly 处理 OpenAPI 3.0 到 Swagger 2.0 转换时的特殊规则：
// 如果一个属性既是 required（必需）又是 readonly（只读），则从 required 列表中移除，只保留 readonly 标记。
// 这是因为 Swagger 2.0 规范不允许 required 属性同时是 readonly。
// 映射关系：schema.Required[] -> 过滤后的 schema.Required[]（移除所有 readonly 属性）
func make30RequiredAndReadonlyPropertiesOnlyReadonly(schema *base.Schema) {
	if schema.Properties != nil && len(schema.Required) > 0 {
		newRequired := []string{}

		for _, propName := range schema.Required {
			readonly := false

			if schema.Properties != nil {
				if item, ok := schema.Properties.Get(propName); ok {
					propSchema := item.Schema()

					readonly = propSchema.ReadOnly != nil && *propSchema.ReadOnly
				}
			}

			if !readonly {
				newRequired = append(newRequired, propName)
			}
		}

		schema.Required = newRequired
	}
}

// convert30NullablesTo31TypeArrays 将 OpenAPI 3.0 的 nullable 字段映射到 OpenAPI 3.1 的 type 数组。
// 映射关系：
//   - OpenAPI 3.0: {type: "string", nullable: true} -> OpenAPI 3.1: {type: ["string", "null"]}
//   - OpenAPI 3.0: {type: "string", nullable: false} -> OpenAPI 3.1: {type: ["string"]}（nullable 字段被移除）
//
// 操作：将 schema.Nullable 的值转换为 schema.Type 数组中的 "null" 元素，然后清空 schema.Nullable
func convert30NullablesTo31TypeArrays(schema *base.Schema) {
	// Replace {type: T, nullable: true} with {type: [T, "null"]}, etc.
	if schema.Nullable != nil {
		if *schema.Nullable {
			schema.Type = append(schema.Type, "null")
		}

		schema.Nullable = nil
	}
}

// convert31TypeArraysTo30 将 OpenAPI 3.1 的 type 数组映射回 OpenAPI 3.0 的 nullable 字段或 oneOf。
// 映射关系：
//   - OpenAPI 3.1: {type: ["string", "null"]} -> OpenAPI 3.0: {type: "string", nullable: true}
//   - OpenAPI 3.1: {type: ["string", "integer", "null"]} -> OpenAPI 3.0: {oneOf: [{type: "string", nullable: true}, {type: "integer", nullable: true}]}
//   - OpenAPI 3.1: {type: ["string", "integer"]} -> OpenAPI 3.0: {oneOf: [{type: "string"}, {type: "integer"}]}
//
// 操作：
//   - 如果 type 数组包含 "null" 且只有两个元素，则转换为 {type: T, nullable: true}
//   - 如果 type 数组有多个非 null 元素，则转换为 oneOf 结构
func convert31TypeArraysTo30(schema *base.Schema) {
	nullable := false
	nonNullType := ""

	for _, value := range schema.Type {
		if value == "null" {
			nullable = true
		} else {
			nonNullType = value
		}
	}

	if nullable && len(schema.Type) == 2 {
		// In case of {type: [T, "null"]} set {type: T, nullable: true}
		schema.Type[0] = nonNullType
		schema.Type = schema.Type[:1]
		schema.Nullable = &nullable
	} else if len(schema.Type) >= 2 {
		// In case of 2 or more non-null values, set them in oneOf
		// if "null" was one of the values then all values will be nullable.
		schema.OneOf = make([]*base.SchemaProxy, 0, len(schema.Type))

		for _, value := range schema.Type {
			if value != "null" {
				newSchema := base.Schema{Type: []string{value}}

				if nullable {
					newSchema.Nullable = &nullable
				}

				schema.OneOf = append(schema.OneOf, base.CreateSchemaProxy(&newSchema))
			}
		}

		// Clear the type field.
		schema.Type = nil
	}
}

// convert30MinMaxTo31 将 OpenAPI 3.0 的 minimum/exclusiveMinimum 和 maximum/exclusiveMaximum 字段映射到 OpenAPI 3.1。
// 映射关系：
//   - OpenAPI 3.0: {minimum: 10, exclusiveMinimum: true} -> OpenAPI 3.1: {exclusiveMinimum: 10}（DynamicValue 的 B 字段存储数值）
//   - OpenAPI 3.0: {minimum: 10, exclusiveMinimum: false} -> OpenAPI 3.1: {minimum: 10}（exclusiveMinimum 被移除）
//   - OpenAPI 3.0: {maximum: 100, exclusiveMaximum: true} -> OpenAPI 3.1: {exclusiveMaximum: 100}
//   - OpenAPI 3.0: {maximum: 100, exclusiveMaximum: false} -> OpenAPI 3.1: {maximum: 100}
//
// 操作：
//   - 当 exclusiveMinimum/exclusiveMaximum 为 true 时，将 minimum/maximum 的值移到 exclusiveMinimum/exclusiveMaximum 的 B 字段（数值类型）
//   - 当 exclusiveMinimum/exclusiveMaximum 为 false 时，直接移除该字段
//
// 注意：OpenAPI 3.1 的 exclusiveMinimum/exclusiveMaximum 是 DynamicValue 类型，可以是 bool（A 字段）或 float64（B 字段）
func convert30MinMaxTo31(schema *base.Schema) {
	convert30ExclusiveBoundTo31 := func(
		bound **float64,
		exclusiveBound **base.DynamicValue[bool, float64],
	) {
		if *exclusiveBound != nil && (*exclusiveBound).IsA() {
			if (*exclusiveBound).A {
				// Before: {miniumum: val, exclusiveMinimum: true}
				// After: {exclusiveMinimum: val}
				if *bound != nil {
					(*exclusiveBound).N = 1
					(*exclusiveBound).B = **bound
				}

				*bound = nil
			} else {
				// Before: {minimum: val, exclusiveMinimum: false}
				// After: {minimum: val}
				*exclusiveBound = nil
			}
		}
	}

	convert30ExclusiveBoundTo31(&schema.Minimum, &schema.ExclusiveMinimum)
	convert30ExclusiveBoundTo31(&schema.Maximum, &schema.ExclusiveMaximum)
}

// convert31MinMaxTo30 将 OpenAPI 3.1 的 exclusiveMinimum/exclusiveMaximum 字段映射回 OpenAPI 3.0。
// 映射关系：
//   - OpenAPI 3.1: {exclusiveMinimum: 10}（DynamicValue 的 B 字段为数值）-> OpenAPI 3.0: {minimum: 10, exclusiveMinimum: true}
//   - OpenAPI 3.1: {minimum: 10} -> OpenAPI 3.0: {minimum: 10}（保持不变）
//   - OpenAPI 3.1: {exclusiveMaximum: 100} -> OpenAPI 3.0: {maximum: 100, exclusiveMaximum: true}
//
// 操作：
//   - 当 exclusiveMinimum/exclusiveMaximum 是数值类型（IsB() 返回 true）时，将其值移到 minimum/maximum，并设置 exclusiveMinimum/exclusiveMaximum 为 true
//
// 注意：只处理数值类型的 exclusiveBound（B 字段），bool 类型的（A 字段）在 3.0 中不存在
func convert31MinMaxTo30(schema *base.Schema) {
	convert31ExclusiveBoundTo30 := func(
		bound **float64,
		exclusiveBound **base.DynamicValue[bool, float64],
	) {
		if *exclusiveBound != nil && (*exclusiveBound).IsB() {
			// Before: {exclusiveMinimum: val}
			// After: {minimum: value, exclusiveMinimum: true}
			*bound = &(*exclusiveBound).B
			(*exclusiveBound).A = true
			(*exclusiveBound).N = 0
		}
	}

	convert31ExclusiveBoundTo30(&schema.Minimum, &schema.ExclusiveMinimum)
	convert31ExclusiveBoundTo30(&schema.Maximum, &schema.ExclusiveMaximum)
}

// convert30ExampleTo31Examples 将 OpenAPI 3.0 的 example 字段映射到 OpenAPI 3.1 的 examples 数组。
// 映射关系：
//   - OpenAPI 3.0: {example: value} -> OpenAPI 3.1: {examples: [value]}
//
// 操作：将 schema.Example 的值放入 schema.Examples 数组的第一个位置，然后清空 schema.Example
func convert30ExampleTo31Examples(schema *base.Schema) {
	if schema.Example != nil {
		schema.Examples = []*yaml.Node{schema.Example}
		schema.Example = nil
	}
}

// convert31ExamplesTo30Example 将 OpenAPI 3.1 的 examples 数组映射回 OpenAPI 3.0 的 example 字段。
// 映射关系：
//   - OpenAPI 3.1: {examples: [value1, value2, ...]} -> OpenAPI 3.0: {example: value1}（只取第一个）
//
// 操作：将 schema.Examples 数组的第一个元素赋值给 schema.Example，然后清空 schema.Examples
func convert31ExamplesTo30Example(schema *base.Schema) {
	if len(schema.Examples) >= 1 {
		schema.Example = schema.Examples[0]
		schema.Examples = nil
	}
}

// convert30FormatsTo31ContentFields 将 OpenAPI 3.0 的 format 字段映射到 OpenAPI 3.1 的 contentMediaType 和 contentEncoding 字段。
// 映射关系：
//   - OpenAPI 3.0: {type: "string", format: "binary"} -> OpenAPI 3.1: {type: "string", contentMediaType: "base64"}
//   - OpenAPI 3.0: {type: "string", format: "byte"} -> OpenAPI 3.1: {type: "string", contentMediaType: "base64"}
//   - OpenAPI 3.0: {type: "string", format: "base64"} -> OpenAPI 3.1: {type: "string", contentEncoding: "base64"}
//
// 操作：
//   - 将 format: "binary" 或 "byte" 映射到 lowSchema.ContentMediaType = "base64"
//   - 将 format: "base64" 映射到 lowSchema.ContentEncoding = "base64"
//   - 清空 schema.Format 字段
//
// 注意：此函数需要访问底层 low schema 来设置 contentMediaType 和 contentEncoding
func convert30FormatsTo31ContentFields(schema *base.Schema) {
	if len(schema.Type) == 1 && schema.Type[0] == "string" && len(schema.Format) > 0 {
		if schema.Format == "binary" || schema.Format == "byte" {
			lowSchema := schema.GoLow()

			if lowSchema != nil {
				lowSchema.ContentMediaType = low.NodeReference[string]{
					Value:     "base64",
					ValueNode: utils.CreateStringNode("base64"),
				}
			}
		} else if schema.Format == "base64" {
			lowSchema := schema.GoLow()

			if lowSchema != nil {
				lowSchema.ContentEncoding = low.NodeReference[string]{
					Value:     "base64",
					ValueNode: utils.CreateStringNode("base64"),
				}
			}
		}

		schema.Format = ""
	}
}

// convert31ContentFieldsTo30Formats 将 OpenAPI 3.1 的 contentMediaType 和 contentEncoding 字段映射回 OpenAPI 3.0 的 format 字段。
// 映射关系：
//   - OpenAPI 3.1: {type: "string", contentMediaType: "application/octet-stream"} -> OpenAPI 3.0: {type: "string", format: "binary"}
//   - OpenAPI 3.1: {type: "string", contentEncoding: "base64"} -> OpenAPI 3.0: {type: "string", format: "base64"}
//
// 操作：
//   - 将 lowSchema.ContentMediaType = "application/octet-stream" 映射到 schema.Format = "binary"
//   - 将 lowSchema.ContentEncoding = "base64" 映射到 schema.Format = "base64"
//   - 清空 lowSchema.ContentMediaType 和 lowSchema.ContentEncoding 字段
//
// 注意：此函数需要访问底层 low schema 来读取 contentMediaType 和 contentEncoding
func convert31ContentFieldsTo30Formats(schema *base.Schema) {
	if len(schema.Type) == 1 && schema.Type[0] == "string" {
		lowSchema := schema.GoLow()

		if lowSchema != nil {
			if len(lowSchema.ContentMediaType.Value) > 0 {
				if lowSchema.ContentMediaType.Value == "application/octet-stream" {
					schema.Format = "binary"
				}

				lowSchema.ContentMediaType.Mutate("")
			}

			if len(lowSchema.ContentEncoding.Value) > 0 {
				if lowSchema.ContentEncoding.Value == "base64" {
					schema.Format = "base64"
				}

				lowSchema.ContentEncoding.Mutate("")
			}
		}
	}
}

// updateSchemaAndReferencedSchema 递归更新 schema 及其所有引用的子 schema。
// 遍历路径：
//  1. schema.Properties -> 每个属性的 schema
//  2. schema.Items -> 数组元素的 schema
//  3. schema.AllOf -> 所有组合的 schema
//  4. schema.OneOf -> 任一组合的 schema
//  5. schema.AnyOf -> 任意组合的 schema
//  6. 最后更新当前 schema 本身
//
// 操作：对每个找到的 schema 调用 callback 函数进行转换
func updateSchemaAndReferencedSchema(
	schema *base.Schema,
	callback func(schema *base.Schema),
) {
	if schema == nil {
		// Skip editing nil schema.
		return
	}

	// Handle schemas in properties.
	if schema.Properties != nil {
		for property := range schema.Properties.ValuesFromOldest() {
			callback(property.Schema())
		}
	}

	// Handle items if the schema is an array.
	if schema.Items != nil {
		if schema.Items.IsA() {
			callback(schema.Items.A.Schema())
		}
	}

	// Process composite schemas: allOf, oneOf, and anyOf.
	for _, subSchema := range schema.AllOf {
		callback(subSchema.Schema())
	}

	for _, subSchema := range schema.OneOf {
		callback(subSchema.Schema())
	}

	for _, subSchema := range schema.AnyOf {
		callback(subSchema.Schema())
	}

	// Modify this schema last, so our changes to schema are final.
	callback(schema)
}

// updateAllSchema 在整个 OpenAPI 文档中查找所有 schema 并使用 callback 更新它们。
// 查找位置：
//  1. model.Model.Components.Schemas -> 组件中定义的 schema（全局可复用的 schema）
//  2. model.Model.Components.Parameters -> 参数中的 schema（参数定义中的 schema）
//  3. model.Model.Paths -> 路径操作中的 schema：
//     a. operation.RequestBody.Content -> 请求体的 content 中的 schema
//     b. operation.Responses.Codes -> 响应中的 content 中的 schema
//
// 操作：对每个找到的 schema 调用 updateSchemaAndReferencedSchema 进行递归更新
func updateAllSchema(
	model *libopenapi.DocumentModel[v3.Document],
	callback func(schema *base.Schema),
) {
	if model.Model.Components != nil && model.Model.Components.Schemas != nil {
		for value := range model.Model.Components.Schemas.ValuesFromOldest() {
			updateSchemaAndReferencedSchema(value.Schema(), callback)
		}
	}

	if model.Model.Components != nil && model.Model.Components.Parameters != nil {
		for value := range model.Model.Components.Parameters.ValuesFromOldest() {
			updateSchemaAndReferencedSchema(value.Schema.Schema(), callback)
		}
	}

	if model.Model.Paths != nil && model.Model.Paths.PathItems != nil {
		for pathItem := range model.Model.Paths.PathItems.ValuesFromOldest() {
			for operation := range pathItem.GetOperations().ValuesFromOldest() {
				if operation.RequestBody != nil && operation.RequestBody.Content != nil {
					for content := range operation.RequestBody.Content.ValuesFromOldest() {
						if content.Schema != nil {
							updateSchemaAndReferencedSchema(content.Schema.Schema(), callback)
						}
					}
				}

				if operation.Responses != nil && operation.Responses.Codes != nil {
					for code := range operation.Responses.Codes.ValuesFromOldest() {
						if code.Content != nil {
							for mediaType := range code.Content.ValuesFromOldest() {
								if mediaType.Schema != nil {
									updateSchemaAndReferencedSchema(mediaType.Schema.Schema(), callback)
								}
							}
						}
					}
				}
			}
		}
	}
}

// clear30RequestFileContentSchemaFor31 在 OpenAPI 3.0 到 3.1 转换时，清除文件上传请求体的 schema。
// 映射关系：
//   - OpenAPI 3.0: {content: {"application/octet-stream": {schema: {...}}}}
//     -> OpenAPI 3.1: {content: {"application/octet-stream": {schema: null}}}
//
// 操作：将 content["application/octet-stream"].Schema 设置为 nil
// 原因：在 OpenAPI 3.1 中，application/octet-stream 的 schema 类型是隐式的，不需要显式定义
func clear30RequestFileContentSchemaFor31(
	model *libopenapi.DocumentModel[v3.Document],
) {
	if model.Model.Paths != nil && model.Model.Paths.PathItems != nil {
		for pathItem := range model.Model.Paths.PathItems.ValuesFromOldest() {
			for operation := range pathItem.GetOperations().ValuesFromOldest() {
				if operation.RequestBody != nil && operation.RequestBody.Content != nil {
					// Clear the schema for application/octet-stream, as the type is implied.
					if content, ok := operation.RequestBody.Content.Get("application/octet-stream"); ok {
						content.Schema = nil
					}
				}
			}
		}
	}
}

// set31RequestFileContentSchemaFor30 在 OpenAPI 3.1 到 3.0 转换时，为文件上传请求体添加 schema。
// 映射关系：
//   - OpenAPI 3.1: {content: {"application/octet-stream": {schema: null}}}
//     -> OpenAPI 3.0: {content: {"application/octet-stream": {schema: {type: "string", format: "binary"}}}}
//
// 操作：将 content["application/octet-stream"].Schema 设置为 {type: ["string"], format: "binary"}
// 原因：在 OpenAPI 3.0 中，需要显式定义二进制文件的 schema
func set31RequestFileContentSchemaFor30(
	model *libopenapi.DocumentModel[v3.Document],
) {
	if model.Model.Paths != nil && model.Model.Paths.PathItems != nil {
		for pathItem := range model.Model.Paths.PathItems.ValuesFromOldest() {
			for operation := range pathItem.GetOperations().ValuesFromOldest() {
				if operation.RequestBody != nil && operation.RequestBody.Content != nil {
					// Clear the schema for application/octet-stream, as the type is implied.
					if content, ok := operation.RequestBody.Content.Get("application/octet-stream"); ok {
						content.Schema = base.CreateSchemaProxy(&base.Schema{
							Type:   []string{"string"},
							Format: "binary",
						})
					}
				}
			}
		}
	}
}

// ensureRequestBodyContentSchemas 确保所有请求体 content 都有有效的 schema。
// 映射关系：
//   - {content: {..., schema: null}} -> {content: {..., schema: {type: ["object"]}}}
//
// 操作：如果 content.Schema 为 nil，则创建一个默认的空对象 schema {type: ["object"]}
// 原因：kin-openapi 的 FromV3 转换器无法处理 nil schema，需要为每个 content 提供有效的 schema
func ensureRequestBodyContentSchemas(
	model *libopenapi.DocumentModel[v3.Document],
) {
	if model.Model.Paths != nil && model.Model.Paths.PathItems != nil {
		for pathItem := range model.Model.Paths.PathItems.ValuesFromOldest() {
			for operation := range pathItem.GetOperations().ValuesFromOldest() {
				if operation.RequestBody != nil && operation.RequestBody.Content != nil {
					for content := range operation.RequestBody.Content.ValuesFromOldest() {
						// If schema is nil, create a default empty object schema
						if content.Schema == nil {
							content.Schema = base.CreateSchemaProxy(&base.Schema{
								Type: []string{"object"},
							})
						}
					}
				}
			}
		}
	}
}

// fixSwaggerOperationUploadFormat 修复 Swagger 2.0 操作中文件上传格式的缺失 schema。
// 映射关系：
//   - Swagger 2.0: {consumes: ["application/octet-stream"], parameters: [{in: "body", schema: null}]}
//     -> Swagger 2.0: {consumes: ["application/octet-stream"], parameters: [{in: "body", schema: {type: "string", format: "binary"}}]}
//
// 操作：如果操作 consumes "application/octet-stream" 且 body 参数的 schema 为 nil，则添加 {type: "string", format: "binary"}
// 原因：kin-openapi 转换器在创建上传规范时不会自动添加 schema，需要手动补充
func fixSwaggerOperationUploadFormat(operation *openapi2.Operation) {
	if operation != nil && slices.Contains(operation.Consumes, "application/octet-stream") {
		for _, param := range operation.Parameters {
			if param.In == "body" && param.Schema == nil {
				param.Schema = &openapi2.SchemaRef{
					Value: &openapi2.Schema{
						Type:   &openapi3.Types{"string"},
						Format: "binary",
					},
				}
			}
		}
	}
}

// fixSwaggerDocUploadFormats 修复整个 Swagger 文档中所有操作的文件上传格式。
// 操作范围：
//   - 遍历文档中的所有路径（paths）
//   - 对每个路径的以下操作进行修复：POST、OPTIONS、PATCH、PUT
//   - 注意：HEAD、GET、DELETE 操作不检查（这些操作通常不包含文件上传）
//
// 操作：对每个符合条件的操作调用 fixSwaggerOperationUploadFormat 进行修复
func fixSwaggerDocUploadFormats(kinSwaggerDoc *openapi2.T) {
	for _, path := range kinSwaggerDoc.Paths {
		// HEAD, GET, DELETE we don't check here.
		// All other operations we try to fix.
		fixSwaggerOperationUploadFormat(path.Post)
		fixSwaggerOperationUploadFormat(path.Options)
		fixSwaggerOperationUploadFormat(path.Patch)
		fixSwaggerOperationUploadFormat(path.Put)
	}
}

// copyDescriptionToSummary 处理操作的 summary 和 description 字段映射。
// 映射规则：
//  1. 如果有 summary，使用 summary 映射到 summary 字段（保持不变）
//  2. 如果没有 summary，将 description 映射到 summary 上
//  3. description 映射后每次都追加 gRPC客户端名称和接口方法名称到映射的 description 里
//
// 操作：
//   - 如果 operation.Summary 不为空，保留 summary
//   - 如果 operation.Summary 为空且 operation.Description 不为空，将 description 复制到 summary
//   - 在 description 后面追加 gRPC 客户端名称（从 Tags 获取）和接口方法名称（从 OperationID 提取）
//
// 原因：某些工具或规范要求操作必须有 summary 字段，同时需要在 description 中包含 gRPC 信息
func copyDescriptionToSummary(operation *openapi2.Operation) {
	if operation == nil {
		return
	}

	// 提取 gRPC 客户端名称（从 Tags 的第一个元素）
	grpcClientName := ""
	if len(operation.Tags) > 0 {
		grpcClientName = operation.Tags[0]
	}

	// 提取接口方法名称（从 OperationID，格式通常是 "ServiceName_MethodName"）
	methodName := ""
	if operation.OperationID != "" {
		// 如果 OperationID 包含下划线，提取下划线后的部分作为方法名
		if idx := strings.LastIndex(operation.OperationID, "_"); idx >= 0 && idx < len(operation.OperationID)-1 {
			methodName = operation.OperationID[idx+1:]
		} else {
			// 如果没有下划线，使用整个 OperationID 作为方法名
			methodName = operation.OperationID
		}
	}

	// 构建要追加到 description 的 gRPC 信息
	grpcInfo := ""
	if grpcClientName != "" || methodName != "" {
		var parts []string
		if grpcClientName != "" {
			parts = append(parts, fmt.Sprintf("gRPC客户端名称：%s", grpcClientName))
		}
		if methodName != "" {
			parts = append(parts, fmt.Sprintf("接口方法名称：%s", methodName))
		}
		if len(parts) > 0 {
			grpcInfo = "\n\n" + strings.Join(parts, "\n")
		}
	}

	// 如果有 summary，保留 summary；如果没有，将 description 复制到 summary
	if operation.Summary == "" && operation.Description != "" {
		operation.Summary = operation.Description
	}

	// 在 description 后面追加 gRPC 信息
	if grpcInfo != "" {
		if operation.Description != "" {
			operation.Description = operation.Description + grpcInfo
		} else {
			operation.Description = strings.TrimPrefix(grpcInfo, "\n\n")
		}
	}
}

func deduplicateTags(operation *openapi2.Operation) {
	if operation == nil || len(operation.Tags) == 0 {
		return
	}

	// Use a map to track seen tags and preserve order
	seen := make(map[string]bool)
	uniqueTags := make([]string, 0, len(operation.Tags))

	for _, tag := range operation.Tags {
		if !seen[tag] {
			seen[tag] = true
			uniqueTags = append(uniqueTags, tag)
		}
	}

	operation.Tags = uniqueTags
}

// addDefaultErrorResponseToOperation 为操作添加默认错误响应，引用 rpcStatus schema。
// 映射关系：
//   - {responses: {}} -> {responses: {"default": {description: "...", schema: {ref: "#/definitions/rpcStatus"}}}}
//
// 操作：在 operation.Responses 中添加或更新 "default" 响应，其 schema 引用 "#/definitions/rpcStatus"
// 原因：为所有操作提供统一的错误响应格式，符合 gRPC 规范
func addDefaultErrorResponseToOperation(operation *openapi2.Operation) {
	if operation == nil {
		return
	}

	// Initialize Responses map if it's nil
	if operation.Responses == nil {
		operation.Responses = make(map[string]*openapi2.Response)
	}

	// Always update default error response to use rpcStatus
	operation.Responses["default"] = &openapi2.Response{
		Description: "An unexpected error response.",
		Schema: &openapi2.SchemaRef{
			Ref: "#/definitions/rpcStatus",
		},
	}
}

// addDefaultErrorResponses 为 Swagger 文档添加默认错误响应和相关的 schema 定义。
// 主要操作：
//  1. 确保 definitions 映射存在
//  2. 添加 googleprotobufAny schema 定义（如果不存在）
//  3. 添加或更新 rpcStatus schema 定义（如果不存在）
//  4. 为所有路径的所有操作执行以下操作：
//     a. 将 description 复制到 summary（如果 summary 为空）
//     b. 去重操作 tags
//     c. 添加默认错误响应（引用 rpcStatus）
//
// 映射关系：
//   - definitions -> definitions["googleprotobufAny"]（Google Protobuf Any 类型定义）
//   - definitions -> definitions["rpcStatus"]（gRPC 状态码定义，包含 code、message、details 字段）
//   - operation.Responses -> operation.Responses["default"]（默认错误响应）
func addDefaultErrorResponses(kinSwaggerDoc *openapi2.T) {
	// Ensure definitions map exists
	if kinSwaggerDoc.Definitions == nil {
		kinSwaggerDoc.Definitions = make(map[string]*openapi2.SchemaRef)
	}

	// Add googleprotobufAny definition if it doesn't exist
	if _, exists := kinSwaggerDoc.Definitions["googleprotobufAny"]; !exists {
		kinSwaggerDoc.Definitions["googleprotobufAny"] = &openapi2.SchemaRef{
			Value: &openapi2.Schema{
				Type:        &openapi3.Types{"object"},
				Description: "`Any` contains an arbitrary serialized protocol buffer message along with a\nURL that describes the type of the serialized message.\n\nProtobuf library provides support to pack/unpack Any values in the form\nof utility functions or additional generated methods of the Any type.\n\nExample 1: Pack and unpack a message in C++.\n\n    Foo foo = ...;\n    Any any;\n    any.PackFrom(foo);\n    ...\n    if (any.UnpackTo(&foo)) {\n      ...\n    }\n\nExample 2: Pack and unpack a message in Java.\n\n    Foo foo = ...;\n    Any any = Any.pack(foo);\n    ...\n    if (any.is(Foo.class)) {\n      foo = any.unpack(Foo.class);\n    }\n\nExample 3: Pack and unpack a message in Python.\n\n    foo = Foo(...)\n    any = Any()\n    any.Pack(foo)\n    ...\n    if any.Is(Foo.DESCRIPTOR):\n      any.Unpack(foo)\n      ...\n\nExample 4: Pack and unpack a message in Go\n\n     foo := &pb.Foo{...}\n     any, err := anypb.New(foo)\n     if err != nil {\n       ...\n     }\n     ...\n     foo := &pb.Foo{}\n     if err := any.UnmarshalTo(foo); err != nil {\n       ...\n     }\n\nThe pack methods provided by protobuf library will by default use\n'type.googleapis.com/full.type.name' as the type URL and the unpack\nmethods only use the fully qualified type name after the last '/'\nin the type URL, for example \"foo.bar.com/x/y.z\" will yield type\nname \"y.z\".\n\n\nJSON\n\nThe JSON representation of an `Any` value uses the regular\nrepresentation of the deserialized, embedded message, with an\nadditional field `@type` which contains the type URL. Example:\n\n    package google.profile;\n    message Person {\n      string first_name = 1;\n      string last_name = 2;\n    }\n\n    {\n      \"@type\": \"type.googleapis.com/google.profile.Person\",\n      \"firstName\": <string>,\n      \"lastName\": <string>\n    }\n\nIf the embedded message type is well-known and has a custom JSON\nrepresentation, that representation will be embedded adding a field\n`value` which holds the custom JSON in addition to the `@type`\nfield. Example (for message [google.protobuf.Duration][]):\n\n    {\n      \"@type\": \"type.googleapis.com/google.protobuf.Duration\",\n      \"value\": \"1.212s\"\n    }",
				Properties: map[string]*openapi2.SchemaRef{
					"@type": {
						Value: &openapi2.Schema{
							Type:        &openapi3.Types{"string"},
							Description: "A URL/resource name that uniquely identifies the type of the serialized\nprotocol buffer message. This string must contain at least\none \"/\" character. The last segment of the URL's path must represent\nthe fully qualified name of the type (as in\n`path/google.protobuf.Duration`). The name should be in a canonical form\n(e.g., leading \".\" is not accepted).\n\nIn practice, teams usually precompile into the binary all types that they\nexpect it to use in the context of Any. However, for URLs which use the\nscheme `http`, `https`, or no scheme, one can optionally set up a type\nserver that maps type URLs to message definitions as follows:\n\n* If no scheme is provided, `https` is assumed.\n* An HTTP GET on the URL must yield a [google.protobuf.Type][]\n  value in binary format, or produce an error.\n* Applications are allowed to cache lookup results based on the\n  URL, or have them precompiled into a binary to avoid any\n  lookup. Therefore, binary compatibility needs to be preserved\n  on changes to types. (Use versioned type names to manage\n  breaking changes.)\n\nNote: this functionality is not currently available in the official\nprotobuf release, and it is not used for type URLs beginning with\ntype.googleapis.com.\n\nSchemes other than `http`, `https` (or the empty scheme) might be\nused with implementation specific semantics.",
						},
					},
				},
				AdditionalProperties: openapi3.AdditionalProperties{
					Schema: &openapi3.SchemaRef{
						Value: &openapi3.Schema{},
					},
				},
			},
		}
	}

	// Add or update rpcStatus definition
	if _, exists := kinSwaggerDoc.Definitions["rpcStatus"]; !exists {
		kinSwaggerDoc.Definitions["rpcStatus"] = &openapi2.SchemaRef{
			Value: &openapi2.Schema{
				Type: &openapi3.Types{"object"},
				Properties: map[string]*openapi2.SchemaRef{
					"code": {
						Value: &openapi2.Schema{
							Type:   &openapi3.Types{"integer"},
							Format: "int32",
						},
					},
					"message": {
						Value: &openapi2.Schema{
							Type: &openapi3.Types{"string"},
						},
					},
					"details": {
						Value: &openapi2.Schema{
							Type: &openapi3.Types{"array"},
							Items: &openapi2.SchemaRef{
								Ref: "#/definitions/googleprotobufAny",
							},
						},
					},
				},
			},
		}
	}

	// Copy description to summary, deduplicate tags, and add default error response to all operations
	for _, path := range kinSwaggerDoc.Paths {
		copyDescriptionToSummary(path.Delete)
		copyDescriptionToSummary(path.Get)
		copyDescriptionToSummary(path.Head)
		copyDescriptionToSummary(path.Options)
		copyDescriptionToSummary(path.Patch)
		copyDescriptionToSummary(path.Post)
		copyDescriptionToSummary(path.Put)

		deduplicateTags(path.Delete)
		deduplicateTags(path.Get)
		deduplicateTags(path.Head)
		deduplicateTags(path.Options)
		deduplicateTags(path.Patch)
		deduplicateTags(path.Post)
		deduplicateTags(path.Put)

		addDefaultErrorResponseToOperation(path.Delete)
		addDefaultErrorResponseToOperation(path.Get)
		addDefaultErrorResponseToOperation(path.Head)
		addDefaultErrorResponseToOperation(path.Options)
		addDefaultErrorResponseToOperation(path.Patch)
		addDefaultErrorResponseToOperation(path.Post)
		addDefaultErrorResponseToOperation(path.Put)
	}
}

// convertSwaggerToOpenAPI30 将 Swagger 2.0 文档转换为 OpenAPI 3.0 文档。
// 主要结构映射（由 kin-openapi 库处理）：
//  1. swagger: "2.0" -> openapi: "3.0.x"
//  2. paths -> paths（路径结构基本保持不变，但内部结构有变化）
//  3. definitions -> components.schemas（全局 schema 定义移到 components 下）
//  4. parameters -> components.parameters（全局参数定义移到 components 下）
//  5. responses -> components.responses（全局响应定义移到 components 下）
//  6. securityDefinitions -> components.securitySchemes（安全定义移到 components 下）
//  7. operation.parameters -> operation.requestBody 或 operation.parameters（body 参数转为 requestBody）
//  8. operation.consumes/produces -> operation.requestBody.content / operation.responses[].content（媒体类型映射）
//
// 操作流程：
//  1. 检测输入格式（YAML/JSON），如果是 YAML 则先转换为 JSON（kin-openapi 无法正确解析 YAML）
//  2. 使用 openapispecconverter.UnmarshalSwagger 解析 Swagger 2.0 文档
//  3. 使用 openapi2conv.ToV3 转换为 OpenAPI 3.0 文档
//  4. 返回 JSON 格式的 OpenAPI 3.0 文档
func convertSwaggerToOpenAPI30(data []byte) ([]byte, error) {
	var kinSwaggerDoc openapi2.T

	dataFormat := checkDataFormat(data)

	// kin-openapi cannot unmarshal YAML correctly, so we have to first convert input to JSON.
	if dataFormat != JSON {
		var err error
		data, err = ghodssYaml.YAMLToJSON(data)

		if err != nil {
			return nil, fmt.Errorf("Error converting Swagger YAML to JSON: %w", err)
		}
	}

	if err := openapispecconverter.UnmarshalSwagger(data, &kinSwaggerDoc); err != nil {
		return nil, fmt.Errorf("Error loading Swagger data: %w", err)
	}

	if kinOpenAPIDoc, err := openapi2conv.ToV3(&kinSwaggerDoc); err == nil {
		return kinOpenAPIDoc.MarshalJSON()
	} else {
		return nil, fmt.Errorf("Error converting Swagger to 3.0 %w", err)
	}
}

// convertOpenAPI30ToSwagger 将 OpenAPI 3.0 文档转换为 Swagger 2.0 文档。
// 主要结构映射（由 kin-openapi 库处理）：
//  1. openapi: "3.0.x" -> swagger: "2.0"
//  2. components.schemas -> definitions（组件 schema 移到全局 definitions）
//  3. components.parameters -> parameters（组件参数移到全局 parameters）
//  4. components.responses -> responses（组件响应移到全局 responses）
//  5. components.securitySchemes -> securityDefinitions（安全方案移到全局 securityDefinitions）
//  6. operation.requestBody -> operation.parameters（requestBody 转为 body 参数）
//  7. operation.requestBody.content / operation.responses[].content -> operation.consumes/produces（媒体类型映射）
//
// 字段映射处理：
//  1. schema.Required + schema.ReadOnly -> schema.Required（移除同时为 readonly 的 required 属性）
//  2. content.Schema (nil) -> content.Schema ({type: "object"})（为 nil schema 添加默认值）
//  3. content["application/octet-stream"].Schema -> parameters[].Schema ({type: "string", format: "binary"})（文件上传格式修复）
//  4. operation.Responses -> operation.Responses["default"]（添加默认错误响应）
//  5. definitions -> definitions["rpcStatus"] 和 definitions["googleprotobufAny"]（添加 gRPC 标准定义）
//
// 操作流程：
//  1. 使用 libopenapi 加载并构建 OpenAPI 3.0 文档模型
//  2. 修复 schema 中的 required/readonly 冲突
//  3. 确保所有 requestBody content 都有有效的 schema
//  4. 重新渲染并重新加载文档
//  5. 使用 kin-openapi 的 FromV3 转换为 Swagger 2.0
//  6. 修复文件上传格式和添加默认错误响应
//  7. 返回 JSON 格式的 Swagger 2.0 文档
func convertOpenAPI30ToSwagger(data []byte) ([]byte, error) {
	doc, err := libopenapi.NewDocument(data)

	if err != nil {
		return nil, fmt.Errorf("Error loading document: %w", err)
	}

	// Build the document in libopenapi so we can modify the document
	// to correct issues not handled by kin-openapi.
	model, errs := doc.BuildV3Model()

	if len(errs) > 0 {
		return nil, fmt.Errorf("Errors loading document: %w", errors.Join(errs...))
	}

	updateAllSchema(model, func(schema *base.Schema) {
		// We must make every property that is both required and also readonly
		// only be readonly, or they will break Swagger validation.
		make30RequiredAndReadonlyPropertiesOnlyReadonly(schema)
	})

	// Ensure all request body content has valid schemas before conversion
	// kin-openapi's FromV3 converter cannot handle nil schemas
	ensureRequestBodyContentSchemas(model)

	data, doc, model, errs = doc.RenderAndReload()

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	var kinSwaggerDoc *openapi2.T

	if kinOpenAPIDoc, err := openapi3.NewLoader().LoadFromData(data); err == nil {
		kinSwaggerDoc, err = openapi2conv.FromV3(kinOpenAPIDoc)

		if err != nil {
			return nil, fmt.Errorf("Error converting 3.0 to Swagger %w", err)
		}
	} else {
		return nil, fmt.Errorf("Error Load 3.0 for converting to Swagger %w", err)
	}

	// The kin-openapi Swagger converter doesn't add {schema: {type: "string", format: "binary"}}
	// when creating upload specs for binary content. We need to add it back in again.
	fixSwaggerDocUploadFormats(kinSwaggerDoc)

	// Add default error response to all operations
	addDefaultErrorResponses(kinSwaggerDoc)

	return kinSwaggerDoc.MarshalJSON()
}

// convertOpenAPI30To31 将 OpenAPI 3.0 文档转换为 OpenAPI 3.1 文档。
// 主要字段映射：
//  1. model.Model.Version: "3.0.x" -> "3.1.1"
//  2. schema.Nullable -> schema.Type 数组（添加 "null" 元素）
//  3. schema.Minimum + schema.ExclusiveMinimum (bool) -> schema.ExclusiveMinimum (float64)
//  4. schema.Maximum + schema.ExclusiveMaximum (bool) -> schema.ExclusiveMaximum (float64)
//  5. schema.Example -> schema.Examples 数组
//  6. schema.Format -> lowSchema.ContentMediaType 或 lowSchema.ContentEncoding
//  7. content["application/octet-stream"].Schema -> null（清除）
//
// 参考：https://www.openapis.org/blog/2021/02/16/migrating-from-openapi-3-0-to-3-1-0
func convertOpenAPI30To31(data []byte) ([]byte, error) {
	doc, err := libopenapi.NewDocument(data)

	if err != nil {
		return nil, fmt.Errorf("Error loading document: %w", err)
	}

	model, errs := doc.BuildV3Model()

	if len(errs) > 0 {
		return nil, fmt.Errorf("Errors loading document: %w", errors.Join(errs...))
	}

	// See: https://www.openapis.org/blog/2021/02/16/migrating-from-openapi-3-0-to-3-1-0
	//
	// The following changes need to be made.
	//
	// 1. Change the `openapi` version to 3.1.x.
	// 2. Swap nullable for type arrays.
	// 3. Replace `minimum` and `exclusiveMinimum`, and `maximum` and `exclusiveMaximum`.
	// 4. Replace `example` with `examples` wherever we see it.
	// 5. Modify file upload schemas.

	// 1. Change the `openapi` version to 3.1.x.
	model.Model.Version = "3.1.1"

	// Before scanning all schema, apply step 5. early to clear schema for request bodies.
	clear30RequestFileContentSchemaFor31(model)

	updateAllSchema(model, func(schema *base.Schema) {
		// 2. Swap nullable for type arrays.
		convert30NullablesTo31TypeArrays(schema)
		// 3. Replace `minimum` and `exclusiveMinimum`
		convert30MinMaxTo31(schema)
		// 4. Replace `example` with `examples` wherever we see it.
		convert30ExampleTo31Examples(schema)
		// 5. Modify file upload schemas.
		convert30FormatsTo31ContentFields(schema)
	})

	data, doc, model, errs = doc.RenderAndReload()

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return data, nil
}

// convertOpenAPI31To30 将 OpenAPI 3.1 文档转换为 OpenAPI 3.0 文档。
// 主要字段映射（与 convertOpenAPI30To31 相反）：
//  1. model.Model.Version: "3.1.x" -> "3.0.4"
//  2. schema.Type 数组（包含 "null"）-> schema.Nullable 或 schema.OneOf
//  3. schema.ExclusiveMinimum (float64) -> schema.Minimum + schema.ExclusiveMinimum (bool)
//  4. schema.ExclusiveMaximum (float64) -> schema.Maximum + schema.ExclusiveMaximum (bool)
//  5. schema.Examples 数组 -> schema.Example（只取第一个）
//  6. lowSchema.ContentMediaType / lowSchema.ContentEncoding -> schema.Format
//  7. content["application/octet-stream"].Schema (null) -> content["application/octet-stream"].Schema ({type: "string", format: "binary"})
//  8. model.Model.JsonSchemaDialect -> ""（移除 3.1 特有字段）
//  9. model.Model.Webhooks -> nil（移除 3.1 特有字段）
//  10. model.Model.Info.Summary -> ""（移除 3.1 特有字段）
//
// 操作流程：
//  1. 使用 libopenapi 加载并构建 OpenAPI 3.1 文档模型
//  2. 修改版本号为 3.0.4
//  3. 为文件上传请求体添加 schema
//  4. 递归更新所有 schema：类型数组、最小值/最大值、示例、格式字段
//  5. 移除 3.1 特有的字段（JsonSchemaDialect、Webhooks、Info.Summary）
//  6. 重新渲染并重新加载文档
//  7. 返回转换后的 OpenAPI 3.0 文档
func convertOpenAPI31To30(data []byte) ([]byte, error) {
	doc, err := libopenapi.NewDocument(data)

	if err != nil {
		return nil, fmt.Errorf("Error loading document: %w", err)
	}

	model, errs := doc.BuildV3Model()

	if len(errs) > 0 {
		return nil, fmt.Errorf("Errors loading document: %w", errors.Join(errs...))
	}

	// We need to perform the inverse of the conversion steps in the 3.0 to 3.1 function.

	// 1. Change the `openapi` version to 3.0.x
	model.Model.Version = "3.0.4"

	// Before scanning all schema, apply step 5. early to schema schema for file uploads where needed.
	set31RequestFileContentSchemaFor30(model)

	updateAllSchema(model, func(schema *base.Schema) {
		// 2. Swap type arrays for either `nullable` or `oneOf`
		convert31TypeArraysTo30(schema)
		// 3. Replace `minimum` and `exclusiveMinimum`, and `maximum` and `exclusiveMaximum`.
		convert31MinMaxTo30(schema)
		// 4. Replace `examples` with `example` wherever we see it.
		convert31ExamplesTo30Example(schema)
		// 5. Modify file upload schemas.
		convert31ContentFieldsTo30Formats(schema)
	})

	// We must remove additional properties only used in 3.1.
	model.Model.JsonSchemaDialect = ""
	model.Model.Webhooks = nil

	if model.Model.Info != nil {
		model.Model.Info.Summary = ""
	}

	data, doc, model, errs = doc.RenderAndReload()

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return data, nil
}

// convertDocument 将文档从任意版本转换为目标版本。
// 支持的版本转换路径：
//   - Swagger 2.0 <-> OpenAPI 3.0 <-> OpenAPI 3.1
//   - 可以跨版本转换（例如：Swagger 2.0 -> OpenAPI 3.1 会先转换为 3.0，再转换为 3.1）
//
// 版本识别：
//   - 通过解析文档的 "openapi" 或 "swagger" 字段确定输入版本
//   - Swagger 2.0: swagger: "2.0"
//   - OpenAPI 3.0: openapi: "3.0.0" ~ "3.0.4"
//   - OpenAPI 3.1: openapi: "3.1.0" ~ "3.1.1"
//
// 转换策略：
//   - 如果目标版本高于输入版本，逐步升级（Swagger -> 3.0 -> 3.1）
//   - 如果目标版本低于输入版本，逐步降级（3.1 -> 3.0 -> Swagger）
//   - 每次转换只跨越一个版本，确保转换的准确性
func convertDocument(data []byte, outputVersion SpecVersion) ([]byte, error) {
	// First we'll parse the document in the simplest way to determine the document version.
	type BasicDoc struct {
		OpenAPI string `json:"openapi" yaml:"openapi"`
		Swagger string `json:"swagger" yaml:"swagger"`
	}
	var basicDoc BasicDoc

	if err := yaml.Unmarshal(data, &basicDoc); err != nil {
		return nil, fmt.Errorf("Cannot parse Swagger or OpenAPI document")
	}

	// Get the version string from the Swagger doc if empty.
	if len(basicDoc.OpenAPI) == 0 {
		basicDoc.OpenAPI = basicDoc.Swagger
	}

	// Build the model using libopenapi and determine the input version.
	var inputVersion SpecVersion

	switch basicDoc.OpenAPI {
	case "2.0":
		inputVersion = Swagger
	case "3.0.0", "3.0.1", "3.0.2", "3.0.3", "3.0.4":
		inputVersion = OpenAPI30
	case "3.1.0", "3.1.1":
		inputVersion = OpenAPI31
	default:
		return nil, fmt.Errorf("Unsuppoted input document OpenAPI version: %s", basicDoc.OpenAPI)
	}

	var err error

	// Cycle through document versions until we hit the one we want.
	for inputVersion != outputVersion {
		if inputVersion < outputVersion {
			if inputVersion == Swagger {
				data, err = convertSwaggerToOpenAPI30(data)
				inputVersion = OpenAPI30
			} else {
				data, err = convertOpenAPI30To31(data)
				inputVersion = OpenAPI31
			}
		} else {
			if inputVersion == OpenAPI31 {
				data, err = convertOpenAPI31To30(data)
				inputVersion = OpenAPI30
			} else {
				data, err = convertOpenAPI30ToSwagger(data)
				inputVersion = Swagger
			}
		}

		if err != nil {
			return nil, err
		}
	}

	return data, err
}

// checkDataFormat 检测数据格式是 JSON 还是 YAML。
// 检测逻辑：
//   - 如果第一个非空白字符是 '{'，则判定为 JSON 格式
//   - 否则判定为 YAML 格式
//   - 如果数据全为空白字符，默认返回 YAML
//
// 返回：Format 枚举值（JSON 或 YAML）
func checkDataFormat(data []byte) Format {
	for _, b := range data {
		switch b {
		case '{':
			return JSON
		case ' ', '\t', '\r', '\n':
		default:
			return YAML
		}
	}

	return YAML
}

// main 程序主入口函数，执行 OpenAPI 规范转换的完整流程。
// 执行步骤：
//  1. 解析命令行参数（parseArgs）
//  2. 读取输入文件或标准输入（readInputFile）
//  3. 将文档转换为目标版本（convertDocument）
//  4. 检测输出数据格式，如果与目标格式不匹配则进行格式转换（JSON <-> YAML）
//  5. 将结果写入输出文件或标准输出
//
// 错误处理：
//   - 任何步骤出错都会使用 log.Fatalf 终止程序并输出错误信息
func main() {
	arguments := parseArgs()

	data, err := readInputFile(arguments)

	if err != nil {
		log.Fatalf("Error reading input file %v\n", err)
	}

	data, err = convertDocument(data, arguments.outputTarget)

	if err != nil {
		log.Fatalf("Error converting document: %+v\n", err)
	}

	dataFormat := checkDataFormat(data)

	if dataFormat != arguments.outputFormat {
		if arguments.outputFormat == JSON {
			data, err = ghodssYaml.YAMLToJSON(data)
		} else {
			data, err = ghodssYaml.JSONToYAML(data)
		}

		if err != nil {
			log.Fatalf("Error converting to output format: %v\n", err)
		}
	}

	if len(arguments.outputFilename) > 0 {
		if err = os.WriteFile(arguments.outputFilename, data, 0644); err != nil {
			log.Fatalf("Error writing output file: %v\n", err)
		}
	} else {
		fmt.Println(string(data))
	}
}
