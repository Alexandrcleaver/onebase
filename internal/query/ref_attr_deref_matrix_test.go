package query_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ivantit66/onebase/internal/dbtest"
	"github.com/ivantit66/onebase/internal/metadata"
	"github.com/ivantit66/onebase/internal/query"
	"github.com/ivantit66/onebase/internal/storage"
)

// Разыменование до ССЫЛОЧНОГО реквизита: «Исполнитель.Учётка» (#1784).
//
// Ссылочный реквизит хранится колонкой с суффиксом _id, а компилятор брал её
// по имени реквизита — запрос падал сырым «no such column: ref_исполнитель.учётка»,
// хотя по простому реквизиту («Исполнитель.Наименование») то же разыменование
// работало. Тест матричный: проверяет исполнение SQL, а не его текст.

var (
	derefУчётка1 = uuid.MustParse("00000000-0000-4000-8000-000000001784")
	derefУчётка2 = uuid.MustParse("00000000-0000-4000-8000-000000001785")
)

func derefEntities() []*metadata.Entity {
	return []*metadata.Entity{
		{Name: "Учётка", Kind: metadata.KindCatalog, Fields: []metadata.Field{
			{Name: "Наименование", Type: metadata.FieldTypeString},
		}},
		{Name: "Сотрудник", Kind: metadata.KindCatalog, Fields: []metadata.Field{
			{Name: "Наименование", Type: metadata.FieldTypeString},
			{Name: "Учётка", Type: "reference:Учётка", RefEntity: "Учётка"},
			{Name: "Owner", Type: metadata.FieldTypeString},
		}},
		{Name: "ЗадачаДереф", Kind: metadata.KindDocument, Fields: []metadata.Field{
			{Name: "Номер", Type: metadata.FieldTypeString},
			{Name: "Исполнитель", Type: "reference:Сотрудник", RefEntity: "Сотрудник"},
		}},
	}
}

// Два сотрудника со своими учётками, у каждого по задаче. Второй сотрудник —
// «чужой» для построчного фильтра.
func seedDeref(t *testing.T, db *storage.DB) []*metadata.Entity {
	t.Helper()
	ctx := context.Background()
	ents := derefEntities()
	if err := db.Migrate(ctx, ents); err != nil {
		t.Fatalf("миграция: %v", err)
	}
	учётка, сотрудник, задача := ents[0], ents[1], ents[2]
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{derefУчётка1, "первая"}, {derefУчётка2, "вторая"}} {
		if err := db.Upsert(ctx, учётка.Name, u.id, map[string]any{"Наименование": u.name}, учётка); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct {
		name, owner, номер string
		учётка             uuid.UUID
	}{{"Первый", "свой", "З-1", derefУчётка1}, {"Второй", "чужой", "З-2", derefУчётка2}} {
		id := uuid.New()
		if err := db.Upsert(ctx, сотрудник.Name, id,
			map[string]any{"Наименование": s.name, "Учётка": s.учётка, "Owner": s.owner}, сотрудник); err != nil {
			t.Fatal(err)
		}
		if err := db.Upsert(ctx, задача.Name, uuid.New(),
			map[string]any{"Номер": s.номер, "Исполнитель": id}, задача); err != nil {
			t.Fatal(err)
		}
	}
	return ents
}

func derefNumbers(t *testing.T, db *storage.DB, q string, opts query.CompileOpts) []string {
	t.Helper()
	opts.Dialect = db.Dialect()
	r, err := query.Compile(q, opts)
	if err != nil {
		t.Fatalf("компиляция: %v", err)
	}
	rows, err := db.Query(context.Background(), r.SQL, r.Args...)
	if err != nil {
		t.Fatalf("исполнение: %v\nSQL: %s", err, r.SQL)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var номер string
		if err := rows.Scan(&номер); err != nil {
			t.Fatalf("скан: %v\nSQL: %s", err, r.SQL)
		}
		got = append(got, номер)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("строки: %v\nSQL: %s", err, r.SQL)
	}
	return got
}

func assertNumbers(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("получено %v, ожидалось %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("получено %v, ожидалось %v", got, want)
		}
	}
}

func TestRefAttrDereferenceMatrix(t *testing.T) {
	dbtest.ForEachDialect(t, func(t *testing.T, db *storage.DB) {
		ents := seedDeref(t, db)

		t.Run("ссылочный реквизит в условии", func(t *testing.T) {
			got := derefNumbers(t, db,
				`ВЫБРАТЬ Номер ИЗ Документ.ЗадачаДереф ГДЕ Исполнитель.Учётка = &Учётка`,
				query.CompileOpts{Entities: ents, Params: map[string]any{"Учётка": derefУчётка1}})
			assertNumbers(t, got, "З-1")
		})

		t.Run("ссылочный реквизит в выборке", func(t *testing.T) {
			r, err := query.Compile(
				`ВЫБРАТЬ Номер, Исполнитель.Учётка КАК Учётка ИЗ Документ.ЗадачаДереф УПОРЯДОЧИТЬ ПО Номер`,
				query.CompileOpts{Entities: ents, Dialect: db.Dialect()})
			if err != nil {
				t.Fatalf("компиляция: %v", err)
			}
			rows, err := db.Query(context.Background(), r.SQL, r.Args...)
			if err != nil {
				t.Fatalf("исполнение: %v\nSQL: %s", err, r.SQL)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				var номер string
				var учётка any
				if err := rows.Scan(&номер, &учётка); err != nil {
					t.Fatalf("скан: %v\nSQL: %s", err, r.SQL)
				}
				if учётка == nil {
					t.Fatalf("у задачи %s пустая учётка исполнителя\nSQL: %s", номер, r.SQL)
				}
				n++
			}
			if n != 2 {
				t.Fatalf("строк %d, ожидалось 2\nSQL: %s", n, r.SQL)
			}
		})

		t.Run("простой реквизит не изменился", func(t *testing.T) {
			got := derefNumbers(t, db,
				`ВЫБРАТЬ Номер ИЗ Документ.ЗадачаДереф ГДЕ Исполнитель.Наименование = "Второй"`,
				query.CompileOpts{Entities: ents})
			assertNumbers(t, got, "З-2")
		})

		// Права на присоединённую сущность разыменование не обходит: построчный
		// фильтр справочника сотрудников действует и на путь через ссылку.
		t.Run("построчный фильтр присоединённой сущности", func(t *testing.T) {
			opts := query.CompileOpts{
				Entities: ents,
				RowFilters: map[query.SourceRef]*storage.Predicate{
					{Kind: "catalog", Name: "Сотрудник"}: {Field: "Owner", Op: "eq", Value: "свой"},
				},
			}
			q := `ВЫБРАТЬ Номер ИЗ Документ.ЗадачаДереф ГДЕ Исполнитель.Учётка = &Учётка`
			opts.Params = map[string]any{"Учётка": derefУчётка2}
			assertNumbers(t, derefNumbers(t, db, q, opts))
			opts.Params = map[string]any{"Учётка": derefУчётка1}
			assertNumbers(t, derefNumbers(t, db, q, opts), "З-1")
		})
	})
}

// Присоединённая сущность попадает в источники запроса — по ним проверяются
// права роли (план 54): без права на «Сотрудник» запрос не выполнится.
func TestRefAttrDereferenceReportsJoinedSource(t *testing.T) {
	res, err := query.Compile(
		`ВЫБРАТЬ Номер ИЗ Документ.ЗадачаДереф ГДЕ Исполнитель.Учётка = &Учётка`,
		query.CompileOpts{Entities: derefEntities()})
	if err != nil {
		t.Fatalf("компиляция: %v", err)
	}
	if !hasSource(res.Sources, "catalog", "Сотрудник") {
		t.Fatalf("присоединённый справочник не в источниках: %+v", res.Sources)
	}
}
