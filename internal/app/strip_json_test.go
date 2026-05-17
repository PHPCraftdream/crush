package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Fork-only tests (orchestrator UX): stripJSONEnvelope is the safety
// net behind --json / --format json. The behaviours below mirror the
// real failure modes seen against shamir-db audit runs.

func TestStripJSONEnvelope_AlreadyClean(t *testing.T) {
	in := `{"findings":[]}`
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, in, cleaned)
	assert.Empty(t, notes)
}

func TestStripJSONEnvelope_FencedWithoutPreamble(t *testing.T) {
	in := "```json\n{\"a\":1}\n```"
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, `{"a":1}`, cleaned)
	assert.Empty(t, notes, "no preamble or suffix → no notes")
}

func TestStripJSONEnvelope_FencedWithPreambleAndSuffix(t *testing.T) {
	in := "Here is the audit result:\n\n```json\n{\"k\":\"v\"}\n```\n\nLet me know if you need more detail."
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, `{"k":"v"}`, cleaned)
	assert.Contains(t, notes, "Here is the audit result")
	assert.Contains(t, notes, "Let me know if you need")
}

func TestStripJSONEnvelope_NoFenceButProsePreamble(t *testing.T) {
	in := "I have all the data I need. Final answer:\n\n{\"findings\":2}"
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, `{"findings":2}`, cleaned)
	assert.Contains(t, notes, "I have all the data")
}

func TestStripJSONEnvelope_ArrayValue(t *testing.T) {
	in := "Result:\n[1, 2, 3]"
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, "[1, 2, 3]", cleaned)
	assert.Equal(t, "Result:", notes)
}

func TestStripJSONEnvelope_NestedBraces(t *testing.T) {
	in := `{"a":{"b":{"c":1}},"d":[1,2,{"e":"}"}]}`
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, in, cleaned)
	assert.Empty(t, notes)
}

func TestStripJSONEnvelope_BraceInsideString(t *testing.T) {
	// The closing '}' inside the string must NOT terminate the value.
	in := `{"msg":"hello } world"}`
	cleaned, _ := stripJSONEnvelope(in)
	assert.Equal(t, in, cleaned)
}

func TestStripJSONEnvelope_EscapedQuoteInsideString(t *testing.T) {
	in := `{"msg":"she said \"hi\""}`
	cleaned, _ := stripJSONEnvelope(in)
	assert.Equal(t, in, cleaned)
}

func TestStripJSONEnvelope_UnbalancedReturnsOriginal(t *testing.T) {
	in := "Here is broken JSON: {\"k\":\"v"
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, in, cleaned, "unbalanced → return original untouched")
	assert.Empty(t, notes)
}

func TestStripJSONEnvelope_NoJSONAtAll(t *testing.T) {
	in := "the model just wrote prose, no JSON at all"
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, in, cleaned)
	assert.Empty(t, notes)
}

func TestStripJSONEnvelope_RealShamirDBExample(t *testing.T) {
	// Reproducer of the user-reported failure mode: model emits prose
	// preamble + ```json fence around the actual answer.
	in := `У меня есть все данные. Теперь я соберу окончательный JSON. Вот мой анализ:

` + "```json" + `
{"findings":[{"severity":"medium","id":"§C2","loc":"executor.rs:199"}]}
` + "```"
	cleaned, notes := stripJSONEnvelope(in)
	assert.Equal(t, `{"findings":[{"severity":"medium","id":"§C2","loc":"executor.rs:199"}]}`, cleaned)
	assert.Contains(t, notes, "У меня есть все данные")
}
