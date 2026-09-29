package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ivantit66/onebase/internal/auth"
	"github.com/ivantit66/onebase/internal/metadata"
)

// editable_admin_only — запрет по тому, КТО смотрит, а не по данным записи.
// Поэтому он проверяется во всех трёх местах, где может исчезнуть: в разметке,
// в ответе события формы и на записи. Последнее — главное: разметка запрета
// это подсказка интерфейсу, а не защита.
func adminOnlyFixture(t *testing.T) (*Server, *metadata.Entity, uuid.UUID) {
	t.Helper()
	typeField := &metadata.FormElement{
		Kind: metadata.FormElementField, Name: "ПолеТипЗвонка", DataPath: "Объект.ТипЗвонка",
		EditableAdminOnly: true,
		// Ложное readonly_when: по данным записи поле открыто, и именно этот
		// случай раньше снимал запрет после первого же события формы.
		ReadOnlyWhen: `Наименование = "заперт"`,
	}
	button := &metadata.FormElement{
		Kind: metadata.FormElementButton, Name: "Кн",
		Handlers: map[metadata.FormEventType]string{metadata.FormEventOnClick: "Пусто"},
	}
	form := managedObjectForm(
		fieldEl("ПолеНаименование", "Объект.Наименование"),
		typeField,
		fieldEl("ПолеКомментарий", "Объект.Комментарий"),
		button,
	)
	form.ProgramAST = mustParse(t, "Процедура Пусто() КонецПроцедуры")
	ent := &metadata.Entity{
		Name: "Звонок", Kind: metadata.KindCatalog,
		Fields: []metadata.Field{
			{Name: "Наименование", Type: metadata.FieldTypeString},
			{Name: "ТипЗвонка", Type: metadata.FieldTypeString, Default: "Входящий по умолчанию"},
			{Name: "Комментарий", Type: metadata.FieldTypeString},
		},
		Forms: []*metadata.FormModule{form},
	}
	srv, ctx := newSubmitTestServer(t, []*metadata.Entity{ent})
	srv.authRepo = auth.NewRepo(srv.store)
	id := uuid.New()
	if err := srv.store.Upsert(ctx, ent.Name, id, map[string]any{
		"Наименование": "звонок", "ТипЗвонка": "Входящий", "Комментарий": "исходно",
	}, ent); err != nil {
		t.Fatal(err)
	}
	return srv, ent, id
}

func adminOnlyOperator(entityName string) *auth.User {
	return &auth.User{Login: "anna", Roles: []*auth.Role{{Permissions: auth.Permission{
		Catalogs: map[string][]string{entityName: {"read", "write"}},
	}}}}
}

func inputTagByName(t *testing.T, html, name string) string {
	t.Helper()
	marker := `name="` + name + `"`
	at := strings.Index(html, marker)
	if at < 0 {
		t.Fatalf("в разметке нет поля %q", name)
	}
	start := strings.LastIndex(html[:at], "<")
	end := strings.Index(html[at:], ">")
	if start < 0 || end < 0 {
		t.Fatalf("не удалось выделить тег поля %q", name)
	}
	return html[start : at+end]
}

func TestEditableAdminOnlyLocksMarkupForNonAdmin(t *testing.T) {
	_, ent, _ := adminOnlyFixture(t)
	form := ent.Forms[0]
	values := map[string]string{"Наименование": "звонок", "ТипЗвонка": "Входящий", "Комментарий": "исходно"}

	locked := renderFormKeysHTML(t, ent, form, values, nil, false)
	if !strings.Contains(inputTagByName(t, locked, "ТипЗвонка"), "readonly") {
		t.Error("неадминистратор получил редактируемое поле")
	}
	if strings.Contains(inputTagByName(t, locked, "Комментарий"), "readonly") {
		t.Error("заперлось соседнее поле, которое запрета не просило")
	}

	open := renderFormKeysHTML(t, ent, form, values, nil, true)
	if strings.Contains(inputTagByName(t, open, "ТипЗвонка"), "readonly") {
		t.Error("администратору поле осталось запертым")
	}
}

func TestEditableAdminOnlyDropsForgedValueOnWrite(t *testing.T) {
	srv, ent, id := adminOnlyFixture(t)

	// Подделанный POST: разметка запрета клиенту не указ, значение приходит.
	body := url.Values{
		"Наименование": {"звонок"},
		"ТипЗвонка":    {"Исходящий"},
		"Комментарий":  {"правка оператора"},
	}
	req := reqWithChi(http.MethodPost, "/ui/catalog/"+ent.Name+"/"+id.String(), body,
		map[string]string{"entity": ent.Name, "id": id.String()})
	req = req.WithContext(auth.ContextWithUser(req.Context(), adminOnlyOperator(ent.Name)))
	recorder := httptest.NewRecorder()
	srv.submitEdit(recorder, req)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("запись не прошла: %d %s", recorder.Code, recorder.Body.String())
	}

	row, err := srv.store.GetByID(t.Context(), ent.Name, id, ent)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(row["ТипЗвонка"]); got != "Входящий" {
		t.Fatalf("подделанное значение запертого поля записалось: %q", got)
	}
	if got := fmt.Sprint(row["Комментарий"]); got != "правка оператора" {
		t.Fatalf("обычное поле не записалось: %q", got)
	}

	// Администратору то же поле доступно — запрет адресный, а не глухой.
	adminBody := url.Values{
		"Наименование": {"звонок"},
		"ТипЗвонка":    {"Исходящий"},
		"Комментарий":  {"правка оператора"},
	}
	adminReq := reqWithChi(http.MethodPost, "/ui/catalog/"+ent.Name+"/"+id.String(), adminBody,
		map[string]string{"entity": ent.Name, "id": id.String()})
	adminReq = adminReq.WithContext(auth.ContextWithUser(adminReq.Context(), &auth.User{Login: "root", IsAdmin: true}))
	adminRecorder := httptest.NewRecorder()
	srv.submitEdit(adminRecorder, adminReq)
	if adminRecorder.Code != http.StatusSeeOther {
		t.Fatalf("админская запись не прошла: %d %s", adminRecorder.Code, adminRecorder.Body.String())
	}
	row, err = srv.store.GetByID(t.Context(), ent.Name, id, ent)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(row["ТипЗвонка"]); got != "Исходящий" {
		t.Fatalf("администратор не смог изменить поле: %q", got)
	}
}

func TestEditableAdminOnlyForgedNewValuePreservesDefault(t *testing.T) {
	for _, test := range []struct {
		name string
		body url.Values
	}{
		{"поле не прислано", url.Values{"Наименование": {"новый"}, "Комментарий": {"обычное поле"}}},
		{"поле подделано", url.Values{"Наименование": {"новый"}, "ТипЗвонка": {"Подделка"}, "Комментарий": {"обычное поле"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, ent, _ := adminOnlyFixture(t)
			req := reqWithChi(http.MethodPost, "/ui/catalog/"+ent.Name+"/new", test.body,
				map[string]string{"entity": ent.Name})
			req = req.WithContext(auth.ContextWithUser(req.Context(), adminOnlyOperator(ent.Name)))
			rec := httptest.NewRecorder()
			srv.submit(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("запись не прошла: %d %s", rec.Code, rec.Body.String())
			}
			location, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatalf("некорректный адрес записи: %v", err)
			}
			id, err := uuid.Parse(path.Base(location.Path))
			if err != nil {
				t.Fatalf("нет id новой записи: %v", err)
			}
			row, err := srv.store.GetByID(t.Context(), ent.Name, id, ent)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(row["ТипЗвонка"]); got != "Входящий по умолчанию" {
				t.Fatalf("защищённый дефолт изменился: %q", got)
			}
			if got := fmt.Sprint(row["Комментарий"]); got != "обычное поле" {
				t.Fatalf("обычное поле не записалось: %q", got)
			}
		})
	}
}

func TestEditableAdminOnlyStaysLockedAfterFormEvent(t *testing.T) {
	srv, ent, id := adminOnlyFixture(t)
	body := url.Values{
		"_element": {"Кн"}, "_event": {string(metadata.FormEventOnClick)},
		"_kind": {"object"}, "_id": {id.String()},
		"Наименование": {"звонок"}, "ТипЗвонка": {"Входящий"}, "Комментарий": {"исходно"},
	}
	recorder := runFormEventAs(t, srv, ent, body, adminOnlyOperator(ent.Name))
	if recorder.Code != http.StatusOK {
		t.Fatalf("событие формы не отработало: %d %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ElementStates *struct {
			ReadOnly map[string]bool `json:"readonly"`
		} `json:"elementStates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("разбор ответа: %v; тело=%s", err, recorder.Body.String())
	}
	if response.ElementStates == nil || !response.ElementStates.ReadOnly["ПолеТипЗвонка"] {
		t.Fatalf("после события запрет исчез: %s", recorder.Body.String())
	}
	if response.ElementStates.ReadOnly["ПолеКомментарий"] {
		t.Fatalf("запрет расползся на соседнее поле: %s", recorder.Body.String())
	}

	adminRecorder := runFormEventAs(t, srv, ent, body, &auth.User{Login: "root", IsAdmin: true})
	if adminRecorder.Code != http.StatusOK {
		t.Fatalf("админское событие не отработало: %d %s", adminRecorder.Code, adminRecorder.Body.String())
	}
	var adminResponse struct {
		ElementStates *struct {
			ReadOnly map[string]bool `json:"readonly"`
		} `json:"elementStates"`
	}
	if err := json.Unmarshal(adminRecorder.Body.Bytes(), &adminResponse); err != nil {
		t.Fatalf("разбор админского ответа: %v", err)
	}
	if adminResponse.ElementStates != nil && adminResponse.ElementStates.ReadOnly["ПолеТипЗвонка"] {
		t.Fatalf("администратору поле заперлось: %s", adminRecorder.Body.String())
	}
}

func TestEditableAdminOnlyRejectsForgedValueBeforeFormEventWrite(t *testing.T) {
	srv, ent, id := adminOnlyFixture(t)
	ent.Forms[0].ProgramAST = mustParse(t, `
Процедура Пусто()
    Объект.Записать();
КонецПроцедуры`)
	body := url.Values{
		"_element": {"Кн"}, "_event": {string(metadata.FormEventOnClick)},
		"_kind": {"object"}, "_id": {id.String()},
		"Наименование": {"звонок"}, "ТипЗвонка": {"Подделка"},
		"Комментарий": {"из события"},
	}
	rec := runFormEventAs(t, srv, ent, body, adminOnlyOperator(ent.Name))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("событие не прошло: %d %s", rec.Code, rec.Body.String())
	}
	row, err := srv.store.GetByID(t.Context(), ent.Name, id, ent)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(row["ТипЗвонка"]); got != "Входящий" {
		t.Fatalf("обработчик записал подделанное значение: %q", got)
	}
	if got := fmt.Sprint(row["Комментарий"]); got != "из события" {
		t.Fatalf("обычное поле не записалось: %q", got)
	}
}

func TestEditableAdminOnlyRejectsForgedValueOnCloseIntent(t *testing.T) {
	srv, ent, id := adminOnlyFixture(t)
	body := closeIntentBody(uuid.NewString(), "ok", "звонок")
	body.Set("_close_mode", "save")
	body.Set("_id", id.String())
	body.Set("ТипЗвонка", "Подделка")
	body.Set("Комментарий", "при закрытии")
	rec := executeFormCloseIntentAsUser(t, srv, ent, body, adminOnlyOperator(ent.Name))
	if rec.Code != http.StatusOK {
		t.Fatalf("закрытие не прошло: %d %s", rec.Code, rec.Body.String())
	}
	row, err := srv.store.GetByID(t.Context(), ent.Name, id, ent)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(row["ТипЗвонка"]); got != "Входящий" {
		t.Fatalf("закрытие записало подделанное значение: %q", got)
	}
}
