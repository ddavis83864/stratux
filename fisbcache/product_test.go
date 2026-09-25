package fisbcache

import "testing"

func TestClassifyProductID(t *testing.T) {
	cases := []struct {
		id   uint32
		want ProductClass
	}{
		{413, ClassText},
		{63, ClassNexradTile},
		{64, ClassNexradTile},
		{0, ClassUnsupported},   // METAR-as-product-ID - never structurally decoded (see doc comment)
		{8, ClassUnsupported},   // NOTAM - decodeAirmet is dead code
		{101, ClassUnsupported}, // Lightning - never decoded
		{9999, ClassUnsupported},
	}
	for _, c := range cases {
		if got := ClassifyProductID(c.id); got != c.want {
			t.Errorf("ClassifyProductID(%d) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestTextKey(t *testing.T) {
	k := TextKey(TextProductMETAR, "KSEA")
	if k.Class != ClassText || k.Identity != "METAR KSEA" {
		t.Errorf("got %+v", k)
	}
}

func TestTextKey_DistinctLocationsAreDistinctKeys(t *testing.T) {
	a := TextKey(TextProductMETAR, "KSEA")
	b := TextKey(TextProductMETAR, "KPDX")
	if a == b {
		t.Fatal("expected different stations to produce different keys")
	}
}

func TestTextKey_DistinctTypesAreDistinctKeys(t *testing.T) {
	a := TextKey(TextProductMETAR, "KSEA")
	b := TextKey(TextProductTAF, "KSEA")
	if a == b {
		t.Fatal("expected METAR and TAF for the same station to produce different keys")
	}
}
