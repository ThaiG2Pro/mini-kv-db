package txn

import (
	"errors"
	"fmt"

	"minidb/internal/btree"
	"minidb/internal/db"
)

// vacuum.go trả lời câu hỏi mà phase 2 đặt ra và để ngỏ: **vì sao Postgres
// cần VACUUM?**
//
// Câu trả lời, giờ đã là code: vì trong MVCC, một lần UPDATE không sửa dữ
// liệu — nó THÊM một bản mới. Không ai dọn thì mỗi lần ghi lại một khóa là
// một lần chuỗi version dài thêm, và ở bản này chuỗi phải nhét vừa một entry
// B+Tree (~2KB) nên khóa đó cuối cùng KHÔNG GHI ĐƯỢC NỮA. Trong Postgres
// không có trần cứng ấy nên hậu quả nhẹ hơn nhưng âm thầm hơn: file phình mãi
// (table bloat) và mọi scan phải đọc qua rác.
//
// Có hai bộ dọn, và cần cả hai:
//
//	cơ hội (HOT prune) : chạy trong Txn.apply, khi page đã ở trong tay. Gần
//	                     như miễn phí, nhưng chỉ chạm những khóa ĐANG ĐƯỢC GHI.
//	toàn cây (Vacuum)  : chạy ở đây. Chạm cả những khóa không ai ghi nữa —
//	                     mà đúng chỗ đó mới là nơi rác nằm lâu nhất.
//
// Điểm ngặt của cả hai là như nhau: **horizon**. Một transaction chỉ đọc mà
// mở lâu sẽ ghim horizon lại và làm mọi bộ dọn thành vô ích. Đây chính là
// `idle_in_transaction` của Postgres, và nó đo được ở
// TestVacuumBlockedByOldReader.

// VacuumStats là kết quả một lần dọn.
type VacuumStats struct {
	Horizon        uint64
	KeysScanned    int
	KeysRewritten  int
	KeysReclaimed  int
	VersionsBefore int
	VersionsAfter  int
	BytesBefore    int
	BytesAfter     int
}

// VersionsPruned là số version đã bỏ đi.
func (v VacuumStats) VersionsPruned() int { return v.VersionsBefore - v.VersionsAfter }

// vacuumBatch là số khóa mỗi transaction vật lý. Không dọn cả cây trong một
// transaction: log của nó sẽ to bằng cả file, và một lần crash giữa vacuum sẽ
// bắt recovery undo toàn bộ. Chia lô là cách để "dọn dở" vẫn là một trạng thái
// hợp lệ — vacuum không có gì phải nguyên tử cả.
const vacuumBatch = 256

// Vacuum dọn toàn cây một lượt.
//
// Không chạy nền: chạy nền cần một goroutine sống suốt đời Store, và với
// một dự án học thì một hàm gọi được từ test và từ cmd/txnlab nói được nhiều
// hơn. Nợ P6-* ghi lại việc thiếu bộ dọn nền, cùng họ với nợ P5-1 (thiếu
// page cleaner) — hai lần cùng một bài học: cái gì không có người dọn thì
// lớn mãi.
func (s *Store) Vacuum() (VacuumStats, error) {
	st := VacuumStats{Horizon: s.horizon()}

	// Thu thập khóa trước, ghi sau. Vừa duyệt cây vừa sửa nó là chỗ mà cursor
	// của phase 4 không hứa gì cả (nó thả pin giữa hai bước Next).
	var keys [][]byte
	err := s.d.Range([]byte{0x01}, nil, func(k, raw []byte) bool {
		if reserved(k) {
			return true
		}
		st.KeysScanned++
		c, derr := DecodeChain(raw)
		if derr != nil {
			return true // để lô ghi báo lỗi, ở đây chỉ đếm
		}
		st.VersionsBefore += len(c)
		st.BytesBefore += len(raw)
		if _, n := c.Prune(st.Horizon); n > 0 || c.Dead(st.Horizon) {
			keys = append(keys, append([]byte(nil), k...))
		} else {
			st.VersionsAfter += len(c)
			st.BytesAfter += len(raw)
		}
		return true
	})
	if err != nil {
		return st, err
	}

	for i := 0; i < len(keys); i += vacuumBatch {
		hi := i + vacuumBatch
		if hi > len(keys) {
			hi = len(keys)
		}
		if err := s.vacuumKeys(keys[i:hi], &st); err != nil {
			return st, err
		}
	}
	return st, nil
}

func (s *Store) vacuumKeys(keys [][]byte, st *VacuumStats) error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	// Đọc lại horizon cho mỗi lô: transaction cũ có thể đã kết thúc trong lúc
	// ta dọn lô trước, và khi đó lô này dọn được nhiều hơn. Đi theo horizon
	// mới luôn AN TOÀN vì horizon chỉ tăng.
	horizon := s.horizon()
	if horizon < st.Horizon {
		horizon = st.Horizon
	}
	return s.d.Update(func(ptx *db.Txn) error {
		for _, k := range keys {
			raw, err := ptx.Get(k)
			if errors.Is(err, btree.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			c, err := DecodeChain(raw)
			if err != nil {
				return fmt.Errorf("txn: vacuum khóa %q: %w", k, err)
			}
			nc, _ := c.Prune(horizon)
			if nc.Dead(horizon) {
				if err := ptx.Delete(k); err != nil {
					return err
				}
				st.KeysReclaimed++
				s.st.keysReclaimed.Add(1)
				continue
			}
			if len(nc) == len(c) {
				st.VersionsAfter += len(c)
				st.BytesAfter += len(raw)
				continue
			}
			buf := nc.Encode(make([]byte, 0, nc.EncodedSize()))
			if err := ptx.Put(k, buf); err != nil {
				return err
			}
			st.KeysRewritten++
			st.VersionsAfter += len(nc)
			st.BytesAfter += len(buf)
			s.st.verPruned.Add(int64(len(c) - len(nc)))
		}
		return nil
	})
}

// ChainStats soi độ dài chuỗi version của cả cây — con số duy nhất nói được
// "MVCC đang phình bao nhiêu".
type ChainStats struct {
	Keys       int
	Versions   int
	MaxChain   int
	Tombstones int
	Bytes      int
	Reserved   int
	BadChains  int
}

func (s *Store) ChainStats() (ChainStats, error) {
	var cs ChainStats
	err := s.d.Range(nil, nil, func(k, raw []byte) bool {
		if reserved(k) {
			cs.Reserved++
			return true
		}
		cs.Keys++
		cs.Bytes += len(raw)
		c, err := DecodeChain(raw)
		if err != nil {
			cs.BadChains++
			return true
		}
		cs.Versions += len(c)
		if len(c) > cs.MaxChain {
			cs.MaxChain = len(c)
		}
		for _, v := range c {
			if v.Deleted {
				cs.Tombstones++
			}
		}
		return true
	})
	return cs, err
}
