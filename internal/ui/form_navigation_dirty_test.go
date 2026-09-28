package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ivantit66/onebase/internal/dsl/interpreter"
	"github.com/ivantit66/onebase/internal/metadata"
	"github.com/ivantit66/onebase/internal/processor"
	"github.com/ivantit66/onebase/internal/runtime"
	"github.com/ivantit66/onebase/internal/storage"
)

// Обработчик может изменить объект и тут же вызвать ОткрытьФорму. Сервер
// отвечает одновременно navigation и dirty=true, а клиент раньше смотрел только
// на прежний флаг формы: исходно чистая форма уходила по ссылке, и изменение
// обработчика терялось — вопреки обещанию «несохранённая форма остаётся на
// месте».

// Сервер: ответ формы обработки несёт и переход, и признак несохранённых
// изменений — на этой паре держится решение клиента.
func TestOpenForm_ProcessorFormReportsHandlerChangeWithNavigation(t *testing.T) {
	направления := &metadata.Entity{
		Name: "Направления", Kind: metadata.KindCatalog,
		Fields: []metadata.Field{{Name: "Наименование", Type: metadata.FieldTypeString}},
	}
	form := processorExecutionForm(&metadata.FormElement{
		Kind: metadata.FormElementButton, Name: "Открыть",
		Handlers: map[metadata.FormEventType]string{metadata.FormEventOnClick: "ОткрытьНажатие"},
	})
	form.ProgramAST = mustParse(t, `
Процедура ОткрытьНажатие()
	Объект.Имя = "изменил обработчик";
	ОткрытьФорму(Справочники.Направления.НайтиПоНаименованию("Север"));
КонецПроцедуры
`)
	proc := &processor.Processor{
		Name:   "ПоискНаправленияСИменем",
		Params: []processor.Param{{Name: "Имя", Type: "string"}},
		Forms:  []*metadata.FormModule{form},
	}

	ctx := context.Background()
	db, err := storage.ConnectSQLite(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, []*metadata.Entity{направления}); err != nil {
		t.Fatal(err)
	}
	if err := db.Upsert(ctx, направления.Name, uuid.New(), map[string]any{"Наименование": "Север"}, направления); err != nil {
		t.Fatal(err)
	}
	registry := runtime.NewRegistry()
	registry.Load(runtime.LoadOptions{Entities: []*metadata.Entity{направления}})
	registry.LoadProcessors([]*processor.Processor{proc})
	interp := interpreter.New()
	interp.LookupProc = registry.GetModuleProc
	srv := &Server{
		store: db, reg: registry, interp: interp,
		lockMgr: runtime.NewLockManager(), messages: NewMessageStore(), ops: newOperationLimiter(),
	}
	srv.entitySvc = srv.newEntityService(nil)

	rec := postProcessorFormEventExecution(t, srv, proc.Name,
		"application/x-www-form-urlencoded; charset=utf-8", strings.NewReader(processorClickBody("Открыть").Encode()))
	resp := decodeFormEventResponse(t, rec.Body.Bytes())
	if !resp.OK {
		t.Fatalf("ok=false, error=%q", resp.Error)
	}
	if resp.Navigation == nil {
		t.Fatalf("обработчик вызвал ОткрытьФорму, а перехода в ответе нет: %s", rec.Body.String())
	}
	if resp.Dirty == nil || !*resp.Dirty {
		t.Fatalf("обработчик изменил объект, а ответ не несёт dirty=true: %s", rec.Body.String())
	}
}

// Клиент: настоящий dispatchFormEvent из managed.js в детерминированном
// стенде. Решение о переходе принимается по состоянию формы ПОСЛЕ ответа.
func TestManagedNavigationRespectsResponseDirtyState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the browser-side navigation regression test")
	}
	dir := t.TempDir()
	managedPath := filepath.Join(dir, "managed.js")
	if err := os.WriteFile(managedPath, managedJS, 0o600); err != nil {
		t.Fatal(err)
	}
	const harness = `
const fs = require('fs');
const source = fs.readFileSync(process.argv[2], 'utf8');
const start = source.indexOf('  async function dispatchFormEvent(snapshot){');
const end = source.indexOf('\n  window.obFire = function', start);
if (start < 0 || end < 0) throw new Error('dispatchFormEvent slice not found');
const fnSource = source.slice(start, end);

async function run(response, formDirty) {
  const calls = { assign: [], flash: [], dirty: [], values: [] };
  const win = {
    _obFormDirty: formDirty,
    location: { assign(url) { calls.assign.push(url); } },
    obSetManagedFormDirty(v) { calls.dirty.push(v); win._obFormDirty = v; },
    obManagedApplyTablePartRefOptions() {},
    applyTableParts() {},
    obOpenQuestion() {},
  };
  const env = {
    window: win,
    reloadRequired: false, formEventWriteUnknown: false, manualReconcileRequired: false,
    DOC_ID: 'doc', URL: '/ui/form-event',
    fetch: async () => ({ ok: true, json: async () => response }),
    flash: (m, kind) => calls.flash.push([m, kind]),
    setManagedFormDirty: (v) => calls.dirty.push(v),
    applySavedIdentity() {}, openItemPicker() {}, applyFormConditionalCSS() {},
    applyElementStates() {}, applyChoiceList() {}, applyFormTables() {},
    applyValues: (v) => { if (v) calls.values.push(v); },
    refreshQueuedFormEventIdentity() {},
  };
  const factory = new Function(...Object.keys(env), fnSource + '\nreturn dispatchFormEvent;');
  const dispatch = factory(...Object.values(env));
  await dispatch({ body: new URLSearchParams(), form: null, elementName: 'Открыть', extraParams: null, wasNew: false });
  return calls;
}

function check(cond, msg) { if (!cond) throw new Error(msg); }
const nav = { url: '/ui/catalog/Направления/1' };
const blocked = 'Форма содержит несохранённые изменения — переход не выполнен';

(async () => {
  // Чистая форма, обработчик ничего не менял — переход.
  let c = await run({ ok: true, navigation: nav }, false);
  check(c.assign.length === 1 && c.assign[0] === nav.url, 'чистая форма не перешла: ' + JSON.stringify(c));

  // Чистая до ответа форма, но обработчик изменил объект (dirty=true) —
  // остаёмся, изменение видно, флаг поднят, сообщение последним.
  c = await run({ ok: true, dirty: true, navigation: nav, values: { 'Имя': 'изменил обработчик' } }, false);
  check(c.assign.length === 0, 'переход потерял изменение обработчика: ' + JSON.stringify(c));
  check(c.dirty.includes(true), 'флаг несохранённых изменений не поднят: ' + JSON.stringify(c));
  check(c.values.length === 1 && c.values[0]['Имя'] === 'изменил обработчик', 'изменение обработчика не показано: ' + JSON.stringify(c));
  check(c.flash.length > 0 && c.flash[c.flash.length - 1][0] === blocked, 'нет сообщения об отмене перехода: ' + JSON.stringify(c));

  // Правки пользователя до обработчика — переход отменён, ответ применён.
  c = await run({ ok: true, navigation: nav, values: { 'Имя': 'x' } }, true);
  check(c.assign.length === 0, 'несохранённая форма ушла по ссылке: ' + JSON.stringify(c));
  check(c.values.length === 1, 'ответ не применён при отмене перехода: ' + JSON.stringify(c));

  // Обработчик сам записал объект (dirty=false + version) — форма чиста.
  c = await run({ ok: true, dirty: false, version: 2, navigation: nav }, true);
  check(c.assign.length === 1, 'записанная обработчиком форма не перешла: ' + JSON.stringify(c));
  console.log('ok');
})().catch((e) => { console.error(e && e.stack || e); process.exit(1); });
`
	harnessPath := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harnessPath, managedPath).CombinedOutput()
	if err != nil {
		t.Fatalf("стенд managed.js: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("стенд managed.js не дошёл до конца:\n%s", out)
	}
}
