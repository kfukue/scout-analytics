package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The scored row gets the quote symbol live pre-call found only when the view
// has none yet; a symbol the view already has, or an unknown one, leaves the
// row as it is.
func TestFillQuoteAsset(t *testing.T) {
	cases := []struct {
		name    string
		row     string
		sym     string
		want    any  // quote_asset in the result (nil = JSON null or absent)
		same    bool // the row comes back byte for byte
		wantErr bool
	}{
		{name: "null filled", row: `{"call_id":7,"quote_asset":null,"holders":1150}`, sym: "WETH", want: "WETH"},
		{name: "empty filled", row: `{"call_id":7,"quote_asset":"","holders":1150}`, sym: "WETH", want: "WETH"},
		{name: "absent filled", row: `{"call_id":7,"holders":1150}`, sym: "USDG", want: "USDG"},
		{name: "symbol kept as read", row: `{"quote_asset":null}`, sym: "wETH<&>", want: "wETH<&>"},
		{name: "view value kept", row: `{"call_id":7,"quote_asset":"USDG"}`, sym: "WETH", want: "USDG", same: true},
		{name: "unknown symbol leaves null", row: `{"call_id":7,"quote_asset":null}`, sym: "", want: nil, same: true},
		{name: "unknown symbol leaves bad row", row: `not json`, sym: "", same: true},
		{name: "bad row", row: `not json`, sym: "WETH", wantErr: true},
		{name: "not an object", row: `null`, sym: "WETH", wantErr: true},
		{name: "quote_asset not a string", row: `{"quote_asset":5}`, sym: "WETH", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := fillQuoteAsset([]byte(c.row), c.sym)
			if c.wantErr {
				if err == nil {
					t.Fatalf("fillQuoteAsset(%s, %q) = %s, want an error", c.row, c.sym, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("fillQuoteAsset(%s, %q): %v", c.row, c.sym, err)
			}
			if c.same {
				if !bytes.Equal(out, []byte(c.row)) {
					t.Fatalf("fillQuoteAsset(%s, %q) = %s, want the row unchanged", c.row, c.sym, out)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("fillQuoteAsset(%s, %q) = %s: not JSON: %v", c.row, c.sym, out, err)
			}
			if got["quote_asset"] != c.want {
				t.Fatalf("fillQuoteAsset(%s, %q): quote_asset = %v, want %v", c.row, c.sym, got["quote_asset"], c.want)
			}
			var in map[string]any
			if err := json.Unmarshal([]byte(c.row), &in); err != nil {
				t.Fatal(err)
			}
			for k, v := range in { // every other column passes through
				if k != "quote_asset" && got[k] != v {
					t.Fatalf("fillQuoteAsset(%s, %q): %s = %v, want %v", c.row, c.sym, k, got[k], v)
				}
			}
		})
	}
}
