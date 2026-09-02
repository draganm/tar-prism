package tarprysm

// payload pads data to a whole number of blocks, as it appears in an archive.
func payload(data []byte) []byte {
	return append(append([]byte{}, data...), make([]byte, padding(int64(len(data))))...)
}

// endMarker is the two zero blocks that end an archive.
var endMarker = make([]byte, 2*blockSize)

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
