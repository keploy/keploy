package mongo

func hasSecondSetBit(num int) bool {
	// Shift the number right by 1 bit and check if the least significant bit is set
	return (num>>1)&1 == 1
}
