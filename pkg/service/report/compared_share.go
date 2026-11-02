package report

import "go.keploy.io/server/v3/pkg/models"

// reportsShare is the share of the run the reports are of
// (models.ComparedShare): how many of its tests got a verdict, and why the
// rest were set aside. Nil when none was.
func reportsShare(reports map[string]*models.TestReport) *models.ComparedShare {
	var compared, total, setAside int
	counts := map[string]int{}
	for _, rep := range reports {
		if rep == nil {
			continue
		}
		compared += rep.Success + rep.Failure + rep.Obsolete
		total += rep.Total
		setAside += rep.Ignored
		for reason, n := range models.ComparedShareOf(rep).ReasonCounts() {
			counts[reason] += n
		}
	}
	return models.NewComparedShare(compared, total, setAside, counts)
}

// recountShown makes a report's totals those of the rows --test-case left
// it, so what is said of it (its counts, its share) is of what it shows.
func recountShown(rep *models.TestReport) {
	rep.Total = len(rep.Tests)
	rep.Success, rep.Failure, rep.Ignored, rep.Obsolete = 0, 0, 0, 0
	for _, t := range rep.Tests {
		switch t.Status {
		case models.TestStatusPassed:
			rep.Success++
		case models.TestStatusFailed:
			rep.Failure++
		case models.TestStatusIgnored:
			rep.Ignored++
		case models.TestStatusObsolete:
			rep.Obsolete++
		}
	}
}
