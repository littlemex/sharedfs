package quota

import "testing"

func TestLimits(t *testing.T) {
	out := `Disk quotas for prj 4242 (pid 4242):
     Filesystem  kbytes   quota   limit   grace   files   quota   limit   grace
            /fs   52369*      0   51200       -       0       0    1000       -
pid 4242 is using default file quota setting
`
	if got, ok := limits(out); !ok || got != [2]int64{51200, 1000} {
		t.Fatal(got, ok)
	}
	if _, ok := limits("nothing"); ok {
		t.Fatal("no data line must not parse")
	}
}
