package disclosures

import "fmt"

// disclosurePagination tracks the server-declared extent of a paginated
// disclosure listing. A source must not silently continue if its result set
// changes while it is being read.
type disclosurePagination struct {
	total       int
	nextStart   int
	initialized bool
}

// advance validates one raw page and reports whether it reaches the terminal
// extent. rowCount must be the source's unfiltered response count, rather than
// the number of filings the caller elects to process.
func (p *disclosurePagination) advance(source, totalName string, start, rowCount, total int) (bool, error) {
	if total < 0 {
		return false, fmt.Errorf("%s returned negative %s %d", source, totalName, total)
	}
	if p.initialized && total != p.total {
		return false, fmt.Errorf("%s %s changed from %d to %d at start %d", source, totalName, p.total, total, start)
	}
	if start < 0 || start > total {
		return false, fmt.Errorf("%s returned inconsistent %s %d for start %d", source, totalName, total, start)
	}
	if rowCount < 0 || rowCount > total-start {
		return false, fmt.Errorf("%s returned %d rows beyond %s %d at start %d", source, rowCount, totalName, total, start)
	}
	if rowCount == 0 && start < total {
		return false, fmt.Errorf("%s returned an empty page before %s %d at start %d", source, totalName, total, start)
	}
	if start != p.nextStart {
		return false, fmt.Errorf("%s returned page at start %d, expected %d", source, start, p.nextStart)
	}
	if !p.initialized {
		p.total = total
		p.initialized = true
	}
	p.nextStart = start + rowCount
	return p.nextStart >= p.total, nil
}
