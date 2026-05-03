package segment

const (
	CapBit_ColumnZSTD uint64 = 1 << 0

	LSG_REQUIRED_CAPS_KNOWN uint64 = CapBit_ColumnZSTD
	LSG_OPTIONAL_CAPS_KNOWN uint64 = 0
)

func requiredCapsForCompression(c CompressionType) uint64 {
	if c == CompressionZSTD {
		return CapBit_ColumnZSTD
	}
	return 0
}
