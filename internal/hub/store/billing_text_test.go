package store

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
)

// 价格与币种的格式只在 CheckPriceCurrency 定义：建节点与改节点两个写入口都经它，越界什么都不写；面板的费用汇总
// 因此可以把库里的值当作合法的直接计算。
func TestPriceCurrencyAdmission(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n", Billing: Billing{Price: "12.50", Currency: "USD", Cycle: CycleMonthly}}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		price, currency string
		field           string
	}{
		{"lowercase_currency", "5", "usd", "billing.currency"},
		{"four_letter_currency", "5", "USDT", "billing.currency"},
		{"three_decimals", "12.345", "USD", "billing.price"},
		{"ten_digits", "1234567890", "USD", "billing.price"},
		{"symbol", "$5", "USD", "billing.price"},
		{"full_width_digit", "５", "USD", "billing.price"},
		{"price_without_currency", "5", "", "billing.currency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Billing{Price: tc.price, Currency: tc.currency}
			var textErr BillingTextError
			if _, _, err := s.CreateNode(ctx, NewNode{Name: "x-" + tc.name, Billing: b}, hash(byte(len(tc.name)))); !errors.As(err, &textErr) || textErr.Field != tc.field {
				t.Fatalf("CreateNode err = %v, want BillingTextError on %s", err, tc.field)
			}
			if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "n", TrafficResetDay: DefaultTrafficResetDay, Billing: b}); !errors.As(err, &textErr) || textErr.Field != tc.field {
				t.Fatalf("UpdateNodeTasks err = %v, want BillingTextError on %s", err, tc.field)
			}
		})
	}
	n, err := s.GetNode(ctx, id)
	if err != nil || n.Billing.Price != "12.50" || n.Billing.Currency != "USD" {
		t.Fatalf("rejected writes changed billing: %+v err=%v", n.Billing, err)
	}
	for _, b := range []Billing{{}, {Currency: "EUR"}, {Price: "0", Currency: "JPY"}, {Price: "999999999.99", Currency: "CNY"}} {
		if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "n", TrafficResetDay: DefaultTrafficResetDay, Billing: b}); err != nil {
			t.Fatalf("UpdateNodeTasks(%+v) err = %v", b, err)
		}
	}
}

// v54 把早于写入口校验的价格与币种规范化：去掉首尾空白、币种转大写后合法的就留下规范形；仍不合法的一对一起清空
// （计费周期与到期日不动），库里的每一对都满足 CheckPriceCurrency。
func TestMigrationFromV53NormalizesPriceCurrency(t *testing.T) {
	t.Parallel()
	rows := []struct{ price, currency, wantPrice, wantCurrency string }{
		{"12.50", "USD", "12.50", "USD"},
		{" 12.50 ", " usd ", "12.50", "USD"},
		{"5", "eur", "5", "EUR"},
		{"", "jpy", "", "JPY"},
		{"$5", "USD", "", ""},
		{"5", "", "", ""},
		{"5", "us dollar", "", ""},
		{"12.345", "USD", "", ""},
		{"", "", "", ""},
	}
	s := migrateFrom(t, 53, func(t *testing.T, db *sql.DB) {
		for i, r := range rows {
			if _, err := db.Exec("INSERT INTO node (id, name, token_hash, created_at, price, currency, billing_cycle, expires_on) VALUES (?, ?, ?, 1, ?, ?, 'monthly', '2027-01-01')",
				i+1, "n"+string(rune('a'+i)), []byte{byte(i)}, r.price, r.currency); err != nil {
				t.Fatal(err)
			}
		}
	})
	for i, r := range rows {
		var price, currency, cycle, expires string
		if err := s.r.QueryRow("SELECT price, currency, billing_cycle, expires_on FROM node WHERE id = ?", i+1).Scan(&price, &currency, &cycle, &expires); err != nil {
			t.Fatal(err)
		}
		if price != r.wantPrice || currency != r.wantCurrency || cycle != "monthly" || expires != "2027-01-01" {
			t.Errorf("(%q, %q) migrated to (%q, %q, %q, %q), want (%q, %q, monthly, 2027-01-01)", r.price, r.currency, price, currency, cycle, expires, r.wantPrice, r.wantCurrency)
		}
		if err := CheckPriceCurrency(price, currency); err != nil {
			t.Errorf("migrated pair (%q, %q) fails the write admission: %v", price, currency, err)
		}
	}
}

// v54 只改数据，结构与 v53 相同。
var schemaV54 = slices.Clone(schemaV53)
