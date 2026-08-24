package store

import "context"

// queryHistoryLegacy 以旧的 (items, total, err) 形态包装 QueryHistory，
// 供只关心结果集与总数、不关心游标的既有用例使用。
func (s *Store) queryHistoryLegacy(ctx context.Context, f *HistoryFilter) ([]*HistoryRecord, int, error) {
	f.WithTotal = true
	page, err := s.QueryHistory(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	total := 0
	if page.Total != nil {
		total = *page.Total
	}
	return page.Items, total, nil
}
