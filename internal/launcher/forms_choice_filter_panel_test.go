package launcher

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Execute the panel code from the rendered editor: a server-only round trip
// cannot catch an operator silently rewritten by the browser before POST.
func TestFormsEditor_ChoiceFilterEqualOrEmptyPanel(t *testing.T) {
	_, nodeErr := exec.LookPath("node")
	if nodeErr != nil {
		if _, err := exec.LookPath("/System/Library/Frameworks/JavaScriptCore.framework/Versions/A/Helpers/jsc"); err != nil {
			t.Skip("JavaScript engine is required for the choice filter panel test")
		}
	}
	page := formsEditorScript(t)
	start := strings.Index(page, "function addChoiceFilterEditor(panel, info) {")
	if start < 0 {
		t.Fatal("choice filter panel was not found in rendered editor")
	}
	end := strings.Index(page[start:], "function addElementActions(panel, info) {")
	if end < 0 {
		t.Fatal("choice filter panel boundary was not found in rendered editor")
	}
	const testScript = `
function equal(got, want, message) {
  if (got !== want) throw new Error(message + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
}
class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.handlers = {}; this.value = ''; }
  appendChild(child) { this.children.push(child); return child; }
  addEventListener(name, handler) { this.handlers[name] = handler; }
  set innerHTML(value) { this.children = []; }
}
const edits = [];
const document = { createElement: tag => new Element(tag) };
function Option(label, value) { return { label, value }; }
function mkBtn() { return new Element('button'); }
const _selected = 'elements.0';
function editOp(request) { edits.push(request); }
const panel = new Element('panel');
addChoiceFilterEditor(panel, { choiceFilter: [
  { field: 'Филиал', op: 'eq_or_empty', from: 'Объект.Филиал' }
] });
function all(node) { return [node, ...(node.children || []).flatMap(all)]; }
const nodes = all(panel);
const op = nodes.find(node => node.tag === 'select' &&
  node.children.some(option => option.value === 'eq_or_empty'));
if (!op) throw new Error('the editor must offer eq_or_empty');
equal(op.value, 'eq_or_empty', 'opening the panel must preserve the operator');
const mode = nodes.find(node => node.tag === 'select' &&
  node.children.some(option => option.value === 'value'));
equal(mode.disabled, true, 'eq_or_empty requires a field source');
const field = nodes.find(node => node.tag === 'input' && node.value === 'Филиал');
field.value = 'ДругойФилиал';
field.handlers.change();
equal(edits.length, 1, 'edit count');
equal(edits[0].choice_filter, JSON.stringify([
  { field: 'ДругойФилиал', op: 'eq_or_empty', from: 'Объект.Филиал' }
]), 'submitted conditions');
const secondPanel = new Element('panel');
addChoiceFilterEditor(secondPanel, { choiceFilter: [
  { field: 'is_folder', op: 'eq', value: true }
] });
const secondOp = all(secondPanel).find(node => node.tag === 'select' &&
  node.children.some(option => option.value === 'eq_or_empty'));
secondOp.value = 'eq_or_empty';
secondOp.handlers.change();
equal(edits[1].choice_filter, JSON.stringify([
  { field: 'is_folder', op: 'eq_or_empty', from: 'Объект.' }
]), 'switching operator must not retain a boolean literal');
const secondMode = all(secondPanel).find(node => node.tag === 'select' &&
  node.children.some(option => option.value === 'value'));
equal(secondMode.disabled, true, 'switching to eq_or_empty disables boolean mode');
`
	// eval the function from the page in the same global scope as its DOM stubs.
	script := testScript[:strings.Index(testScript, "const panel =")] + "\neval(" + strconv.Quote(page[start:start+end]) + ");\n" + testScript[strings.Index(testScript, "const panel ="):]
	var cmd *exec.Cmd
	if nodeErr == nil {
		cmd = exec.Command("node", "-e", script)
	} else {
		cmd = exec.Command("/System/Library/Frameworks/JavaScriptCore.framework/Versions/A/Helpers/jsc", "-e", script)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rendered choice filter panel: %v\n%s", err, out)
	}
}
