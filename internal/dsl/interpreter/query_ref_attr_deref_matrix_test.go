package interpreter_test

// Разыменование до ссылочного реквизита в выборке отдаёт ссылку (#1784,
// вариант 1): «Исполнитель.Учётка» — СправочникСсылка.Учётка, а не строка UUID,
// и её можно передать параметром в другой запрос. Проверка — через публичную
// точку входа (модуль с Новый Запрос поверх живой базы) и на обоих движках:
// идентификатор хранится на PostgreSQL колонкой UUID, на SQLite — TEXT.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivantit66/onebase/internal/dbtest"
	"github.com/ivantit66/onebase/internal/metadata"
	"github.com/ivantit66/onebase/internal/storage"
)

func refAttrDerefEntities() []*metadata.Entity {
	return []*metadata.Entity{
		{Name: "Учётка", Kind: metadata.KindCatalog, Fields: []metadata.Field{
			{ID: "u_name", Name: "Наименование", Type: metadata.FieldTypeString},
		}},
		{Name: "Сотрудник", Kind: metadata.KindCatalog, Fields: []metadata.Field{
			{ID: "s_name", Name: "Наименование", Type: metadata.FieldTypeString},
			{ID: "s_acc", Name: "Учётка", Type: "reference:Учётка", RefEntity: "Учётка"},
		}},
		{Name: "ЗадачаДереф", Kind: metadata.KindDocument, Fields: []metadata.Field{
			{ID: "z_num", Name: "Номер", Type: metadata.FieldTypeString},
			{ID: "z_exec", Name: "Исполнитель", Type: "reference:Сотрудник", RefEntity: "Сотрудник"},
		}},
	}
}

func TestQueryRefAttrDereference_ВыборкаОтдаётСсылку(t *testing.T) {
	dbtest.ForEachDialect(t, func(t *testing.T, db *storage.DB) {
		ctx := context.Background()
		ents := refAttrDerefEntities()
		require.NoError(t, db.Migrate(ctx, ents))
		учётка, сотрудник, задача := ents[0], ents[1], ents[2]
		accountID, employeeID := uuid.New(), uuid.New()
		require.NoError(t, db.Upsert(ctx, учётка.Name, accountID, map[string]any{"Наименование": "первая"}, учётка))
		require.NoError(t, db.Upsert(ctx, сотрудник.Name, employeeID,
			map[string]any{"Наименование": "Первый", "Учётка": accountID.String()}, сотрудник))
		require.NoError(t, db.Upsert(ctx, задача.Name, uuid.New(),
			map[string]any{"Номер": "З-1", "Исполнитель": employeeID.String()}, задача))

		const src = `Процедура Тест()
			Запрос = Новый Запрос;
			Запрос.Текст = "ВЫБРАТЬ Номер, Исполнитель.Учётка КАК Учётка ИЗ Документ.ЗадачаДереф";
			Стр = Запрос.Выполнить()[0];
			Отчёт = ТипЗнч(Стр.Учётка) + "|" + Строка(ТипЗнч(Стр.Учётка) = Тип("СправочникСсылка.Учётка"));
			// Ссылка из выборки — параметр запроса с тем же разыменованием.
			Втор = Новый Запрос;
			Втор.Текст = "ВЫБРАТЬ Номер ИЗ Документ.ЗадачаДереф ГДЕ Исполнитель.Учётка = &У";
			Втор.УстановитьПараметр("У", Стр.Учётка);
			Отчёт = Отчёт + "|" + Втор.Выполнить()[0].Номер + "|" + Строка(Стр.Учётка);
			Возврат Отчёт;
		КонецПроцедуры`

		assert.Equal(t, "СправочникСсылка.Учётка|true|З-1|"+accountID.String(),
			runOnDBEntities(t, db, ents, src))
	})
}
