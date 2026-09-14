package elect

import "testing"

func TestBeaconRoundtrip(t *testing.T) {
	src := []byte{0x7a, 0x72, 0x6d, 0x3c, 0xd7, 0x23}
	in := []Lease{
		{MAC: [6]byte{0x02, 1, 2, 3, 4, 5}, IP: [4]byte{10, 0, 2, 15}},
		{MAC: [6]byte{0x02, 6, 7, 8, 9, 10}, IP: [4]byte{10, 0, 2, 16}},
	}
	frame := EncodeBeacon(src, 42, in)

	if !IsBeacon(frame) {
		t.Fatal("frame not recognized as beacon")
	}
	bad := append([]byte(nil), frame...)
	bad[12] = 0x08 // wrong ethertype
	if IsBeacon(bad) {
		t.Fatal("wrong ethertype accepted")
	}
	uni := append([]byte(nil), frame...)
	uni[0] = 0x02 // not broadcast
	if IsBeacon(uni) {
		t.Fatal("unicast frame accepted as beacon")
	}

	seq, out, err := ParseBeacon(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if seq != 42 || len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Fatalf("roundtrip mismatch: seq=%d out=%v", seq, out)
	}

	if _, _, err := ParseBeacon(frame[:20]); err == nil {
		t.Fatal("truncated beacon accepted")
	}

	empty := EncodeBeacon(src, 1, nil)
	if _, leases, err := ParseBeacon(empty); err != nil || len(leases) != 0 {
		t.Fatalf("empty beacon: err=%v leases=%d", err, len(leases))
	}
}
