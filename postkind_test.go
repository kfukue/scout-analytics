package main

import "testing"

func TestPostKind(t *testing.T) {
	ca := "0x2222222222222222222222222222222222222222"
	cases := []struct {
		name, text, want string
	}{
		{"real call (post 10405)", "🚨 EARLY CALL — $AUTOPILOT · robinhood\n💰 called at $45k\n\n📊 Pool Info\n🏛 DEX: Longxyz\n📈 Mcap: $52k", PostKindCall},
		{"sample call post", samplePost, PostKindCall},
		{"update (post 10408)", "⚡ $MURKLE hit 3X called $21k → $63k peak since the call · dyor", PostKindUpdate},
		{"update, decimals, lower case", "$X hit 2.5x", PostKindUpdate},
		{"update, space before X", "hit 10 X", PostKindUpdate},
		{"update with the address in the text", "⚡ $MURKLE hit 2X called $21k → $42k peak since the call · dyor\n" + ca, PostKindUpdate},
		{"update that says called at", "⚡ $MURKLE hit 4x called at $21k → $84k", PostKindUpdate},
		{"call whose body says hit 3x", samplePost + "\nlast one hit 3x in an hour", PostKindCall},
		{"call header only, says hit 3x", "🚨 EARLY CALL — $ABC · robinhood\nthe dev's last token hit 3x", PostKindCall},
		{"call data without the header, says hit 3x", "📈 Mcap: $52k\n👥 Holders: 150\nhit 3x before", PostKindCall},
		{"empty", "", PostKindCall},
		{"blank", " \n", PostKindCall},
		{"plain text with an address", "new one, CA: " + ca, PostKindCall},
		{"no number before x", "this will hit x soon " + ca, PostKindCall},
		{"multiplier without hit", "easy 3x " + ca, PostKindCall},
		{"hit a price, not a multiple", "hit $63k mcap " + ca, PostKindCall},
		{"number runs into a word", "hit 3xyz " + ca, PostKindCall},
		{"white is not hit", "white 3x " + ca, PostKindCall},
	}
	for _, c := range cases {
		if got := postKind(c.text); got != c.want {
			t.Errorf("%s: postKind = %q, want %q", c.name, got, c.want)
		}
	}
}
