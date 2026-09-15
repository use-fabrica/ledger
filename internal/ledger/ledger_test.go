package ledger

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/use-fabrica/ledger/internal/store"
)

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad test amount %q: %v", s, err)
	}
	return d
}

func TestValidateZeroSum(t *testing.T) {
	tests := []struct {
		name    string
		entries []EntryInput
		assets  []string // asset of entries[i]
		wantErr error
	}{
		{
			name:    "two entries net to zero",
			entries: []EntryInput{{Amount: dec(t, "100.00")}, {Amount: dec(t, "-100.00")}},
			assets:  []string{"usd", "usd"},
		},
		{
			name:    "many entries net to zero",
			entries: []EntryInput{{Amount: dec(t, "10")}, {Amount: dec(t, "20")}, {Amount: dec(t, "-15")}, {Amount: dec(t, "-15")}},
			assets:  []string{"usd", "usd", "usd", "usd"},
		},
		{
			name:    "non-zero sum rejected",
			entries: []EntryInput{{Amount: dec(t, "100.00")}, {Amount: dec(t, "-99.99")}},
			assets:  []string{"usd", "usd"},
			wantErr: ErrNonZeroSum,
		},
		{
			name:    "independent groups must each net to zero",
			entries: []EntryInput{{Amount: dec(t, "5")}, {Amount: dec(t, "-5")}, {Amount: dec(t, "7")}},
			assets:  []string{"usd", "usd", "eur"},
			wantErr: ErrNonZeroSum,
		},
		{
			name:    "cross-asset coincidence is not zero-sum",
			entries: []EntryInput{{Amount: dec(t, "5")}, {Amount: dec(t, "-5")}, {Amount: dec(t, "7")}, {Amount: dec(t, "-7")}},
			assets:  []string{"usd", "usd", "eur", "eur"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accounts := make([]store.Account, len(tt.assets))
			for i, assetID := range tt.assets {
				accounts[i] = store.Account{AssetID: assetID}
			}
			err := ValidateZeroSum(tt.entries, accounts)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateZeroSum() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestNetDeltas(t *testing.T) {
	tests := []struct {
		name    string
		entries []EntryInput
		want    map[string]decimal.Decimal
	}{
		{
			name:    "single entry per account",
			entries: []EntryInput{{AccountID: "a", Amount: dec(t, "-100.00")}, {AccountID: "b", Amount: dec(t, "100.00")}},
			want:    map[string]decimal.Decimal{"a": dec(t, "-100.00"), "b": dec(t, "100.00")},
		},
		{
			name: "split entries collapse into one net delta",
			entries: []EntryInput{
				{AccountID: "a", Amount: dec(t, "-30.00")},
				{AccountID: "a", Amount: dec(t, "-70.00")},
				{AccountID: "b", Amount: dec(t, "100.00")},
			},
			want: map[string]decimal.Decimal{"a": dec(t, "-100.00"), "b": dec(t, "100.00")},
		},
		{
			name:    "nets may cancel to zero per account",
			entries: []EntryInput{{AccountID: "a", Amount: dec(t, "5")}, {AccountID: "a", Amount: dec(t, "-5")}},
			want:    map[string]decimal.Decimal{"a": dec(t, "0")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := netDeltas(tt.entries)
			if len(got) != len(tt.want) {
				t.Fatalf("netDeltas() = %v, want %v", got, tt.want)
			}
			for accountID, want := range tt.want {
				d, ok := got[accountID]
				if !ok || !d.Equal(want) {
					t.Fatalf("netDeltas()[%s] = %v, want %s (all: %v)", accountID, d, want, got)
				}
			}
		})
	}
}

func TestPrecisionExceeded(t *testing.T) {
	tests := []struct {
		amount    string
		precision int32
		want      bool
	}{
		{"1.23", 2, false},
		{"1.2", 2, false},
		{"1", 2, false},
		{"1.234", 2, true},
		{"0.001", 2, true},
		{"-100.001", 2, true},
		{"-100.00", 2, false},
		{"0.00000001", 8, false},
		{"0.000000001", 8, true},
		{"5", 0, false},
		{"5.1", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.amount, func(t *testing.T) {
			if got := PrecisionExceeded(dec(t, tt.amount), tt.precision); got != tt.want {
				t.Fatalf("PrecisionExceeded(%s, %d) = %v, want %v", tt.amount, tt.precision, got, tt.want)
			}
		})
	}
}
