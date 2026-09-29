package storage

import (
	"database/sql/driver"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
	sqlite "modernc.org/sqlite"
)

// Точная сумма на SQLite.
//
// Число на SQLite хранится текстом ("0.1"), и встроенный sum() переводит каждое
// значение в double. Деньги от этого расходятся на ровном месте: движения 0.1,
// 0.2 и −0.3 давали остаток 2.78e-17, и отбор «ГДЕ СуммаОстаток <> 0»
// возвращал закрытую позицию. На PostgreSQL колонки NUMERIC, и сумма там точная
// сама по себе — диалекты расходились в самом базовом агрегате учёта.
//
// Встроенную функцию SQLite разрешает перекрыть своей с тем же именем и числом
// аргументов, поэтому исправление одно на всех: СУММА() в запросах, виртуальные
// таблицы Остатки/Обороты/ОстаткиИОбороты, итоги регистров, балансы
// бухрегистра и проверки doctor получают точную сумму без правки генераторов SQL.
//
// Контракт встроенного sum() сохранён: NULL пропускаются, пустой набор даёт
// NULL, сумма целых остаётся INTEGER, переполнение целой суммы — ошибка
// «integer overflow», работает и как оконная функция. Меняется одно: нецелые
// значения складываются в десятичной арифметике, и результат один раз
// округляется до ближайшего double. Тип результата прежний (REAL), поэтому
// сравнения и арифметика над суммой в SQL работают как раньше. Граница —
// вычисления внутри SQL до суммирования: произведение Количество * Цена
// по-прежнему считается в double.
func init() {
	sqlite.MustRegisterFunction("sum", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		MakeAggregate: func(sqlite.FunctionContext) (sqlite.AggregateFunction, error) {
			return &exactSum{}, nil
		},
	})
}

var errSumIntegerOverflow = errors.New("integer overflow")

// exactSum повторяет состояние встроенного sum() SQLite (func.c, SumCtx), но
// нецелую часть ведёт точной десятичной суммой. Пока она помещается в int64,
// сумма хранится в фиксированной точке (fixed × 10^-scale) и не аллоцирует на
// строку; переполнение или значение, которое так не выразить, переводят её в
// decimal до конца агрегата.
type exactSum struct {
	cnt    int64 // непустых значений в агрегате (в окне)
	approx bool  // встречалось нецелое значение: результат REAL
	ovrfl  bool  // сумма целых переполнила int64
	iSum   int64 // сумма, пока все значения целые и без переполнения

	fixed int64
	scale int32
	isBig bool
	big   decimal.Decimal
}

// sumNum — слагаемое: m × 10^-sc либо, если так не выразить, d.
type sumNum struct {
	m    int64
	sc   int32
	d    decimal.Decimal
	useD bool
}

func (s *exactSum) Step(_ *sqlite.FunctionContext, args []driver.Value) error {
	s.add(args[0], false)
	return nil
}

// WindowInverse убирает из окна строку, добавленную раньше, — чтобы
// SUM(...) OVER (...) работал так же, как со встроенной функцией.
func (s *exactSum) WindowInverse(_ *sqlite.FunctionContext, args []driver.Value) error {
	s.add(args[0], true)
	return nil
}

func (s *exactSum) WindowValue(*sqlite.FunctionContext) (driver.Value, error) {
	if s.cnt <= 0 {
		return nil, nil
	}
	if !s.approx {
		return s.iSum, nil
	}
	if s.ovrfl {
		return nil, errSumIntegerOverflow
	}
	total := s.big
	if !s.isBig {
		total = decimal.New(s.fixed, -s.scale)
	}
	f, _ := total.Float64()
	return f, nil
}

func (s *exactSum) Final(*sqlite.FunctionContext) {}

func (s *exactSum) add(v driver.Value, remove bool) {
	n, isInt, ok := sumOperand(v)
	if !ok {
		return // NULL
	}
	if remove {
		s.cnt--
	} else {
		s.cnt++
	}
	if !s.approx {
		if isInt {
			r, overflow := addInt64(s.iSum, n.m)
			if remove {
				r, overflow = subInt64(s.iSum, n.m)
			}
			if !overflow {
				s.iSum = r
				return
			}
			s.ovrfl = true
		}
		// Переход к точной сумме: накопленная целая часть плюс текущее значение.
		s.approx = true
		s.fixed, s.scale = s.iSum, 0
	} else if !isInt {
		// Как у встроенной функции: нецелое значение снимает признак
		// переполнения — результат уже не целый.
		s.ovrfl = false
	}
	s.accumulate(n, remove)
}

func (s *exactSum) accumulate(n sumNum, remove bool) {
	if !s.isBig && !n.useD {
		m := n.m // |m| < 10^18, смена знака безопасна
		if remove {
			m = -m
		}
		if s.addFixed(m, n.sc) {
			return
		}
	}
	if !s.isBig {
		s.isBig = true
		s.big = decimal.New(s.fixed, -s.scale)
	}
	d := n.d
	if !n.useD {
		d = decimal.New(n.m, -n.sc)
	}
	if remove {
		s.big = s.big.Sub(d)
	} else {
		s.big = s.big.Add(d)
	}
}

// addFixed прибавляет m × 10^-sc к сумме в фиксированной точке; false —
// не поместилось в int64, сумма остаётся прежней.
func (s *exactSum) addFixed(m int64, sc int32) bool {
	fixed, scale := s.fixed, s.scale
	if sc > scale {
		var ok bool
		if fixed, ok = mulPow10(fixed, sc-scale); !ok {
			return false
		}
		scale = sc
	}
	v, ok := mulPow10(m, scale-sc)
	if !ok {
		return false
	}
	r, overflow := addInt64(fixed, v)
	if overflow {
		return false
	}
	s.fixed, s.scale = r, scale
	return true
}

// sumOperand приводит значение к числу по правилам sqlite3_value_numeric_type:
// целое (в том числе текст «12») остаётся целым; текст с дробной частью или
// экспонентой и REAL — нецелые. Текст, не являющийся числом, считается нулём и
// делает результат REAL, как у встроенного sum(). ok=false — NULL.
func sumOperand(v driver.Value) (n sumNum, isInt, ok bool) {
	switch x := v.(type) {
	case nil:
		return sumNum{}, false, false
	case int64:
		return sumNum{m: x}, true, true
	case float64:
		return sumFloat(x), false, true
	case string:
		n, isInt = sumText(x)
		return n, isInt, true
	case []byte:
		n, isInt = sumText(x)
		return n, isInt, true
	default:
		return sumNum{}, false, true
	}
}

// sumFloat берёт у REAL кратчайшую десятичную запись — ту, что печатается
// (−0.1 из унарного минуса над текстом «0.1» остаётся −0.1, а не двоичной
// дробью рядом с ней).
func sumFloat(x float64) sumNum {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return sumNum{}
	}
	var buf [32]byte
	if m, sc, _, ok := parseFixed(strconv.AppendFloat(buf[:0], x, 'f', -1, 64)); ok {
		return sumNum{m: m, sc: sc}
	}
	return sumNum{d: decimal.NewFromFloat(x), useD: true}
}

func sumText[T ~string | ~[]byte](s T) (sumNum, bool) {
	if m, sc, hasDot, ok := parseFixed(s); ok {
		return sumNum{m: m, sc: sc}, !hasDot
	}
	// Экспонента, больше 18 цифр — медленный, но точный путь. Целое из 19 цифр
	// SQLite по-прежнему считает целым.
	text := strings.TrimSpace(string(s))
	if i, err := strconv.ParseInt(text, 10, 64); err == nil {
		return sumNum{m: i}, true
	}
	if d, err := decimal.NewFromString(text); err == nil {
		return sumNum{d: d, useD: true}, false
	}
	return sumNum{}, false // не число: ноль, результат REAL
}

// parseFixed разбирает десятичную запись [пробелы][знак]цифры[.цифры][пробелы]
// в m × 10^-sc. Не больше 18 цифр — тогда m гарантированно в int64; всё, что
// длиннее или с экспонентой, отдаётся медленному пути (ok=false).
func parseFixed[T ~string | ~[]byte](s T) (m int64, sc int32, hasDot, ok bool) {
	i, j := 0, len(s)
	for i < j && isSumSpace(s[i]) {
		i++
	}
	for j > i && isSumSpace(s[j-1]) {
		j--
	}
	neg := false
	if i < j && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	digits := 0
	for ; i < j; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if digits == 18 {
				return 0, 0, false, false
			}
			digits++
			m = m*10 + int64(c-'0')
			if hasDot {
				sc++
			}
		case c == '.' && !hasDot:
			hasDot = true
		default:
			return 0, 0, false, false
		}
	}
	if digits == 0 {
		return 0, 0, false, false
	}
	if neg {
		m = -m
	}
	return m, sc, hasDot, true
}

func isSumSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// mulPow10 умножает x на 10^k; false — переполнение int64.
func mulPow10(x int64, k int32) (int64, bool) {
	for ; k > 0; k-- {
		if x > math.MaxInt64/10 || x < math.MinInt64/10 {
			return 0, false
		}
		x *= 10
	}
	return x, true
}

func addInt64(a, b int64) (int64, bool) {
	r := a + b
	return r, (a > 0 && b > 0 && r < 0) || (a < 0 && b < 0 && r >= 0)
}

func subInt64(a, b int64) (int64, bool) {
	r := a - b
	return r, (a >= 0 && b < 0 && r < 0) || (a < 0 && b > 0 && r >= 0)
}
