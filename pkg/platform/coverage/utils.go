package coverage

import "strconv"

func CalCovPercentage(covered, total int) string {
	if total == 0 {
		return "100%"
	}
	return strconv.FormatFloat(float64(covered*100)/float64(total), 'f', 2, 64) + "%"
}
