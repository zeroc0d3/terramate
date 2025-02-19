// Copyright 2022 Mineiros GmbH
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package genhcl implements generate_hcl code generation.
package genhcl

import (
	stdfmt "fmt"
	"path"
	"sort"

	hhcl "github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/mineiros-io/terramate/config"
	"github.com/mineiros-io/terramate/errors"
	"github.com/mineiros-io/terramate/event"
	"github.com/mineiros-io/terramate/hcl"
	"github.com/mineiros-io/terramate/hcl/ast"
	"github.com/mineiros-io/terramate/hcl/fmt"
	"github.com/mineiros-io/terramate/hcl/info"
	"github.com/mineiros-io/terramate/stdlib"

	"github.com/mineiros-io/terramate/hcl/eval"
	"github.com/mineiros-io/terramate/lets"
	"github.com/mineiros-io/terramate/project"
	"github.com/mineiros-io/terramate/stack"
	"github.com/rs/zerolog/log"
	"github.com/zclconf/go-cty/cty"
)

// HCL represents generated HCL code from a single block.
// Is contains parsed and evaluated code on it and information
// about the origin of the generated code.
type HCL struct {
	label     string
	origin    info.Range
	body      string
	condition bool
	asserts   []config.Assert
}

const (
	// Header is the current header string used by generate_hcl code generation.
	Header = "// TERRAMATE: GENERATED AUTOMATICALLY DO NOT EDIT"

	// HeaderV0 is the deprecated header string used by generate_hcl code generation.
	HeaderV0 = "// GENERATED BY TERRAMATE: DO NOT EDIT"
)

const (
	// ErrParsing indicates the failure of parsing the generate_hcl block.
	ErrParsing errors.Kind = "parsing generate_hcl block"

	// ErrContentEval indicates the failure to evaluate the content block.
	ErrContentEval errors.Kind = "evaluating content block"

	// ErrConditionEval indicates the failure to evaluate the condition attribute.
	ErrConditionEval errors.Kind = "evaluating condition attribute"

	// ErrInvalidConditionType indicates the condition attribute
	// has an invalid type.
	ErrInvalidConditionType errors.Kind = "invalid condition type"

	// ErrInvalidDynamicIterator indicates that the iterator of a tm_dynamic block
	// is invalid.
	ErrInvalidDynamicIterator errors.Kind = "invalid tm_dynamic.iterator"

	// ErrInvalidDynamicLabels indicates that the labels of a tm_dynamic block is invalid.
	ErrInvalidDynamicLabels errors.Kind = "invalid tm_dynamic.labels"

	// ErrDynamicAttrsEval indicates that the attributes of a tm_dynamic cant be evaluated.
	ErrDynamicAttrsEval errors.Kind = "evaluating tm_dynamic.attributes"

	// ErrDynamicConditionEval indicates that the condition of a tm_dynamic cant be evaluated.
	ErrDynamicConditionEval errors.Kind = "evaluating tm_dynamic.condition"

	// ErrDynamicAttrsConflict indicates fields of tm_dynamic conflicts.
	ErrDynamicAttrsConflict errors.Kind = "tm_dynamic.attributes and tm_dynamic.content have conflicting fields"
)

// Label of the original generate_hcl block.
func (h HCL) Label() string {
	return h.label
}

// Asserts returns all (if any) of the evaluated assert configs of the
// generate_hcl block. If [HCL.Condition] returns false then assert configs
// will always be empty since they are not evaluated at all in that case.
func (h HCL) Asserts() []config.Assert {
	return h.asserts
}

// Header returns the header of the generated HCL file.
func (h HCL) Header() string {
	return Header + "\n\n"
}

// Body returns a string representation of the HCL code
// or an empty string if the config itself is empty.
func (h HCL) Body() string {
	return string(h.body)
}

// Range returns the range information of the generate_file block.
func (h HCL) Range() info.Range {
	return h.origin
}

// Condition returns the evaluated condition attribute for the generated code.
func (h HCL) Condition() bool {
	return h.condition
}

// Context of the generate_hcl block.
func (h HCL) Context() string {
	return "stack"
}

func (h HCL) String() string {
	return stdfmt.Sprintf("Generating file %q (condition %t) (body %q) (origin %q)",
		h.Label(), h.Condition(), h.Body(), h.Range().HostPath())
}

// Load loads from the file system all generate_hcl for
// a given stack. It will navigate the file system from the stack dir until
// it reaches rootdir, loading generate_hcl and merging them appropriately.
//
// All generate_file blocks must have unique labels, even ones at different
// directories. Any conflicts will be reported as an error.
//
// Metadata and globals for the stack are used on the evaluation of the
// generate_hcl blocks.
//
// The rootdir MUST be an absolute path.
func Load(
	root *config.Root,
	st *config.Stack,
	globals *eval.Object,
	vendorDir project.Path,
	vendorRequests chan<- event.VendorRequest,
) ([]HCL, error) {
	logger := log.With().
		Str("action", "genhcl.Load()").
		Stringer("path", st.Dir).
		Logger()

	logger.Trace().Msg("loading generate_hcl blocks.")

	hclBlocks, err := loadGenHCLBlocks(root, st.Dir)
	if err != nil {
		return nil, errors.E("loading generate_hcl", err)
	}

	logger.Trace().Msg("generating HCL code.")

	var hcls []HCL
	for _, hclBlock := range hclBlocks {
		name := hclBlock.Label
		evalctx := stack.NewEvalCtx(root, st, globals)

		vendorTargetDir := project.NewPath(path.Join(
			st.Dir.String(),
			path.Dir(name)))

		evalctx.SetFunction(
			stdlib.Name("vendor"),
			stdlib.VendorFunc(vendorTargetDir, vendorDir, vendorRequests),
		)

		err := lets.Load(hclBlock.Lets, evalctx.Context)
		if err != nil {
			return nil, err
		}

		condition := true
		if hclBlock.Condition != nil {
			value, err := evalctx.Eval(hclBlock.Condition.Expr)
			if err != nil {
				return nil, errors.E(ErrConditionEval, err)
			}
			if value.Type() != cty.Bool {
				return nil, errors.E(
					ErrInvalidConditionType,
					"condition has type %s but must be boolean",
					value.Type().FriendlyName(),
				)
			}
			condition = value.True()
		}

		if !condition {
			hcls = append(hcls, HCL{
				label:     name,
				origin:    hclBlock.Range,
				condition: condition,
			})

			continue
		}

		asserts := make([]config.Assert, len(hclBlock.Asserts))
		assertsErrs := errors.L()
		assertFailed := false

		for i, assertCfg := range hclBlock.Asserts {
			assert, err := config.EvalAssert(evalctx.Context, assertCfg)
			if err != nil {
				assertsErrs.Append(err)
				continue
			}
			asserts[i] = assert
			if !assert.Assertion && !assert.Warning {
				assertFailed = true
			}
		}

		if err := assertsErrs.AsError(); err != nil {
			return nil, err
		}

		if assertFailed {
			hcls = append(hcls, HCL{
				label:     name,
				origin:    hclBlock.Range,
				condition: condition,
				asserts:   asserts,
			})
			continue
		}

		evalctx.SetFunction(stdlib.Name("hcl_expression"), stdlib.HCLExpressionFunc())

		gen := hclwrite.NewEmptyFile()
		if err := copyBody(gen.Body(), hclBlock.Content.Body, evalctx); err != nil {
			return nil, errors.E(ErrContentEval, err, "generate_hcl %q", name)
		}

		formatted, err := fmt.FormatMultiline(string(gen.Bytes()), hclBlock.Range.HostPath())
		if err != nil {
			panic(errors.E(err,
				"internal error: formatting generated code for generate_hcl %q:%s", name, string(gen.Bytes()),
			))
		}
		hcls = append(hcls, HCL{
			label:     name,
			origin:    hclBlock.Range,
			body:      formatted,
			condition: condition,
			asserts:   asserts,
		})
	}

	sort.SliceStable(hcls, func(i, j int) bool {
		return hcls[i].Label() < hcls[j].Label()
	})

	logger.Trace().Msg("evaluated all blocks with success")
	return hcls, nil
}

type dynBlockAttributes struct {
	attributes *hclsyntax.Attribute
	iterator   *hclsyntax.Attribute
	foreach    *hclsyntax.Attribute
	labels     *hclsyntax.Attribute
	condition  *hclsyntax.Attribute
}

// loadGenHCLBlocks will load all generate_hcl blocks.
// The returned map maps the name of the block (its label)
// to the original block and the path (relative to project root) of the config
// from where it was parsed.
func loadGenHCLBlocks(root *config.Root, cfgdir project.Path) ([]hcl.GenHCLBlock, error) {
	res := []hcl.GenHCLBlock{}
	cfg, ok := root.Lookup(cfgdir)
	if ok && !cfg.IsEmptyConfig() {
		res = append(res, cfg.Node.Generate.HCLs...)
	}

	parentCfgDir := cfgdir.Dir()
	if parentCfgDir == cfgdir {
		return res, nil
	}

	parentRes, err := loadGenHCLBlocks(root, parentCfgDir)
	if err != nil {
		return nil, err
	}

	res = append(res, parentRes...)

	return res, nil
}

// copyBody will copy the src body to the given target, evaluating attributes
// using the given evaluation context.
//
// Scoped traversals, like name.traverse, for unknown namespaces will be copied
// as is (original expression form, no evaluation).
//
// Returns an error if the evaluation fails.
func copyBody(dest *hclwrite.Body, src *hclsyntax.Body, eval hcl.Evaluator) error {
	logger := log.With().
		Str("action", "genhcl.copyBody()").
		Logger()

	logger.Trace().Msg("sorting attributes")

	attrs := ast.SortRawAttributes(ast.AsHCLAttributes(src.Attributes))
	for _, attr := range attrs {
		logger := logger.With().
			Str("attrName", attr.Name).
			Logger()

		logger.Trace().Msg("evaluating.")

		// a generate_hcl.content block must be partially evaluated multiple
		// times then the updates nodes should not be persisted.
		expr := &ast.CloneExpression{
			Expression: attr.Expr.(hclsyntax.Expression),
		}

		newexpr, err := eval.PartialEval(expr)
		if err != nil {
			return errors.E(err, attr.Expr.Range())
		}

		logger.Trace().Str("attribute", attr.Name).Msg("Setting evaluated attribute.")
		dest.SetAttributeRaw(attr.Name, ast.TokensForExpression(newexpr))
	}

	logger.Trace().Msg("appending blocks")

	for _, block := range src.Blocks {
		err := appendBlock(dest, block, eval)
		if err != nil {
			return err
		}
	}

	return nil
}

func appendBlock(target *hclwrite.Body, block *hclsyntax.Block, eval hcl.Evaluator) error {
	if block.Type == "tm_dynamic" {
		return appendDynamicBlocks(target, block, eval)
	}

	targetBlock := target.AppendNewBlock(block.Type, block.Labels)
	if block.Body != nil {
		err := copyBody(targetBlock.Body(), block.Body, eval)
		if err != nil {
			return err
		}
	}
	return nil
}

func appendDynamicBlock(
	destination *hclwrite.Body,
	evaluator hcl.Evaluator,
	genBlockType string,
	attrs dynBlockAttributes,
	contentBlock *hclsyntax.Block,
) error {
	var labels []string
	if attrs.labels != nil {
		labelsVal, err := evaluator.Eval(attrs.labels.Expr)
		if err != nil {
			return errors.E(ErrInvalidDynamicLabels,
				err, attrs.labels.Range(),
				"failed to evaluate tm_dynamic.labels")
		}

		labels, err = hcl.ValueAsStringList(labelsVal)
		if err != nil {
			return errors.E(ErrInvalidDynamicLabels,
				err, attrs.labels.Range(),
				"tm_dynamic.labels is not a string list")
		}
	}

	newblock := destination.AppendBlock(hclwrite.NewBlock(genBlockType, labels))

	attributeNames := map[string]struct{}{}
	if attrs.attributes != nil {
		attrsExpr, err := evaluator.PartialEval(attrs.attributes.Expr)
		if err != nil {
			return errors.E(ErrDynamicAttrsEval, err, attrs.attributes.Range())
		}

		tmAttrs := []tmAttribute{}
		switch objectExpr := attrsExpr.(type) {
		case *hclsyntax.LiteralValueExpr:
			val := objectExpr.Val
			if val.IsNull() {
				return errors.E(ErrParsing, objectExpr.Range(), "attributes is null")
			}
			if !val.Type().IsObjectType() {
				return attrErr(attrs.attributes,
					"tm_dynamic attributes must be an object, got %s instead", val.Type().FriendlyName())
			}
			iter := val.ElementIterator()
			for iter.Next() {
				key, val := iter.Element()
				if key.Type() != cty.String {
					panic("unreachable")
				}
				tmAttrs = append(tmAttrs, tmAttribute{
					name:   key.AsString(),
					tokens: ast.TokensForValue(val),
					info:   objectExpr.Range(),
				})
			}

		case *hclsyntax.ObjectConsExpr:
			for _, item := range objectExpr.Items {
				keyVal, err := evaluator.Eval(item.KeyExpr)
				if err != nil {
					return errors.E(ErrDynamicAttrsEval, err,
						item.KeyExpr.Range(),
						"evaluating tm_dynamic.attributes key")
				}
				if keyVal.Type() != cty.String {
					return errors.E(ErrParsing, item.KeyExpr.Range(),
						"tm_dynamic.attributes key %q has type %q, must be a string",
						keyVal.GoString(),
						keyVal.Type().FriendlyName())
				}

				valExpr, err := evaluator.PartialEval(item.ValueExpr)
				if err != nil {
					return errors.E(
						ErrDynamicAttrsEval,
						item.ValueExpr.Range(),
						"failed to evaluate attribute value: %s",
						ast.TokensForExpression(item.ValueExpr),
					)
				}
				tmAttrs = append(tmAttrs, tmAttribute{
					name:   keyVal.AsString(),
					tokens: ast.TokensForExpression(valExpr),
					info:   item.ValueExpr.Range(),
				})
			}

		default:
			return attrErr(attrs.attributes,
				"tm_dynamic attributes must be an object, got %T instead", attrsExpr)
		}

		err = setBodyAttributes(newblock.Body(), tmAttrs)
		if err != nil {
			return err
		}

		for _, attr := range tmAttrs {
			attributeNames[attr.name] = struct{}{}
		}
	}

	if contentBlock != nil {
		for _, attr := range contentBlock.Body.Attributes {
			if _, ok := attributeNames[attr.Name]; ok {
				return errors.E(
					ErrDynamicAttrsConflict,
					attr.Range(),
					"attribute %s already set by tm_dynamic.attributes",
					attr.Name,
				)
			}
		}
		err := copyBody(newblock.Body(), contentBlock.Body, evaluator)
		if err != nil {
			return err
		}
	}

	return nil
}

type tmAttribute struct {
	name   string
	tokens hclwrite.Tokens
	info   hhcl.Range
}

func setBodyAttributes(body *hclwrite.Body, attrs []tmAttribute) error {
	for _, attr := range attrs {
		if !hclsyntax.ValidIdentifier(attr.name) {
			return errors.E(ErrParsing, attr.info,
				"tm_dynamic.attributes key %q is not a valid HCL identifier",
				attr.name)
		}
		body.SetAttributeRaw(attr.name, attr.tokens)
	}
	return nil
}

func appendDynamicBlocks(target *hclwrite.Body, dynblock *hclsyntax.Block, evaluator hcl.Evaluator) error {
	logger := log.With().
		Str("action", "genhcl.appendDynamicBlock").
		Logger()

	logger.Trace().Msg("appending tm_dynamic block")

	errs := errors.L()
	if len(dynblock.Labels) != 1 {
		errs.Append(errors.E(ErrParsing,
			dynblock.LabelRanges, "tm_dynamic requires a single label"))
	}

	attrs, err := getDynamicBlockAttrs(dynblock)
	errs.Append(err)

	contentBlock, err := getContentBlock(dynblock.Body.Blocks)
	errs.Append(err)

	if contentBlock == nil && attrs.attributes == nil {
		errs.Append(errors.E(ErrParsing, dynblock.Body.Range(),
			"`content` block or `attributes` obj must be defined"))
	}

	if err := errs.AsError(); err != nil {
		return err
	}

	genBlockType := dynblock.Labels[0]

	logger = logger.With().
		Str("genBlockType", genBlockType).
		Logger()

	if attrs.condition != nil {
		condition, err := evaluator.Eval(attrs.condition.Expr)
		if err != nil {
			return errors.E(ErrDynamicConditionEval, err)
		}
		if condition.Type() != cty.Bool {
			return errors.E(ErrDynamicConditionEval, "want boolean got %s", condition.Type().FriendlyName())
		}
		if !condition.True() {
			logger.Trace().Msg("condition is false, ignoring block")
			return nil
		}
	}

	var foreach cty.Value

	if attrs.foreach != nil {
		logger.Trace().Msg("evaluating for_each attribute")

		foreach, err = evaluator.Eval(attrs.foreach.Expr)
		if err != nil {
			return wrapAttrErr(err, attrs.foreach, "evaluating `for_each` expression")
		}

		if !foreach.CanIterateElements() {
			return attrErr(attrs.foreach,
				"`for_each` expression of type %s cannot be iterated",
				foreach.Type().FriendlyName())
		}
	}

	if foreach.IsNull() {
		logger.Trace().Msg("no for_each, generating single block")

		if attrs.iterator != nil {
			return errors.E(ErrInvalidDynamicIterator,
				attrs.iterator.Range(),
				"iterator should not be defined when for_each is omitted")
		}

		return appendDynamicBlock(target, evaluator,
			genBlockType, attrs, contentBlock)
	}

	logger.Trace().Msg("defining iterator name")

	iterator := genBlockType

	if attrs.iterator != nil {
		iteratorTraversal, diags := hhcl.AbsTraversalForExpr(attrs.iterator.Expr)
		if diags.HasErrors() {
			return errors.E(ErrInvalidDynamicIterator,
				attrs.iterator.Range(),
				"dynamic iterator must be a single variable name")
		}
		if len(iteratorTraversal) != 1 {
			return errors.E(ErrInvalidDynamicIterator,
				attrs.iterator.Range(),
				"dynamic iterator must be a single variable name")
		}
		iterator = iteratorTraversal.RootName()
	}

	logger = logger.With().
		Str("iterator", iterator).
		Logger()

	logger.Trace().Msg("generating blocks")

	var tmDynamicErr error

	foreach.ForEachElement(func(key, value cty.Value) (stop bool) {
		evaluator.SetNamespace(iterator, map[string]cty.Value{
			"key":   key,
			"value": value,
		})

		if err := appendDynamicBlock(target, evaluator,
			genBlockType, attrs, contentBlock); err != nil {
			tmDynamicErr = err
			return true
		}

		return false
	})

	evaluator.DeleteNamespace(iterator)
	return tmDynamicErr
}

func getDynamicBlockAttrs(block *hclsyntax.Block) (dynBlockAttributes, error) {
	dynAttrs := dynBlockAttributes{}
	errs := errors.L()

	for name, attr := range block.Body.Attributes {
		switch name {
		case "attributes":
			dynAttrs.attributes = attr
			dynAttrs.attributes.Expr = &ast.CloneExpression{
				Expression: attr.Expr,
			}

		case "for_each":
			dynAttrs.foreach = attr
			dynAttrs.foreach.Expr = &ast.CloneExpression{
				Expression: attr.Expr,
			}
		case "labels":
			dynAttrs.labels = attr
		case "iterator":
			dynAttrs.iterator = attr
		case "condition":
			dynAttrs.condition = attr
		default:
			errs.Append(attrErr(
				attr, "tm_dynamic unsupported attribute %q", name))
		}
	}

	// Unusual but we return the value so further errors can still be added
	// based on properties of the attributes that are valid.
	return dynAttrs, errs.AsError()
}

func getContentBlock(blocks hclsyntax.Blocks) (*hclsyntax.Block, error) {
	var contentBlock *hclsyntax.Block

	errs := errors.L()

	for _, b := range blocks {
		if b.Type != "content" {
			errs.Append(errors.E(ErrParsing,
				b.TypeRange, "unrecognized block %s", b.Type))

			continue
		}

		if contentBlock != nil {
			errs.Append(errors.E(ErrParsing, b.TypeRange,
				"multiple definitions of the `content` block"))

			continue
		}

		contentBlock = b
	}

	if err := errs.AsError(); err != nil {
		return nil, err
	}

	return contentBlock, nil
}

func attrErr(attr *hclsyntax.Attribute, msg string, args ...interface{}) error {
	return errors.E(ErrParsing, attr.Expr.Range(), stdfmt.Sprintf(msg, args...))
}

func wrapAttrErr(err error, attr *hclsyntax.Attribute, msg string, args ...interface{}) error {
	return errors.E(ErrParsing, err, attr.Expr.Range(), stdfmt.Sprintf(msg, args...))
}
