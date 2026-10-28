package localimport

import (
	"encoding/binary"
	"errors"
	"os"
)

// 最小只读 SQLite 读取器：仅为遍历 VS Code 系 state.vscdb 的 ItemTable 而写，
// 支持表 B 树（叶/内部页）、溢出页与 WAL 回放；保持项目零第三方依赖。

type sqliteDB struct {
	data     []byte
	pageSize int
	usable   int
	overlay  map[int][]byte
}

func openSQLite(path string) (*sqliteDB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 100 || string(data[:16]) != "SQLite format 3\x00" {
		return nil, errors.New("不是 SQLite 数据库")
	}
	ps := int(binary.BigEndian.Uint16(data[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 {
		return nil, errors.New("页大小异常")
	}
	db := &sqliteDB{data: data, pageSize: ps, usable: ps - int(data[20]), overlay: map[int][]byte{}}
	if wal, err := os.ReadFile(path + "-wal"); err == nil {
		db.applyWAL(wal)
	}
	return db, nil
}

// applyWAL 回放已提交的 WAL 帧（按 salt 校验，遇到不匹配即停）。
func (db *sqliteDB) applyWAL(wal []byte) {
	if len(wal) < 32 {
		return
	}
	magic := binary.BigEndian.Uint32(wal[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return
	}
	if int(binary.BigEndian.Uint32(wal[8:12])) != db.pageSize {
		return
	}
	salt1, salt2 := binary.BigEndian.Uint32(wal[16:20]), binary.BigEndian.Uint32(wal[20:24])
	frame := 24 + db.pageSize
	pending := map[int][]byte{}
	for off := 32; off+frame <= len(wal); off += frame {
		h := wal[off : off+24]
		if binary.BigEndian.Uint32(h[8:12]) != salt1 || binary.BigEndian.Uint32(h[12:16]) != salt2 {
			break
		}
		pg := int(binary.BigEndian.Uint32(h[0:4]))
		if pg <= 0 {
			break
		}
		pending[pg] = wal[off+24 : off+frame]
		if binary.BigEndian.Uint32(h[4:8]) != 0 { // 提交帧
			for k, v := range pending {
				db.overlay[k] = v
			}
			pending = map[int][]byte{}
		}
	}
}

func (db *sqliteDB) page(n int) []byte {
	if n <= 0 {
		return nil
	}
	if p, ok := db.overlay[n]; ok {
		return p
	}
	start := (n - 1) * db.pageSize
	if start+db.pageSize > len(db.data) {
		return nil
	}
	return db.data[start : start+db.pageSize]
}

func varint(b []byte) (int64, int) {
	var v uint64
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			v = v<<8 | uint64(b[i])
			return int64(v), 9
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return int64(v), i + 1
		}
	}
	return 0, 0
}

type cell struct {
	rowid int64
	total int
	local []byte
	ovfl  int
}

// walk 中序遍历表 B 树；visit 返回 false 终止。
func (db *sqliteDB) walk(pg, depth int, visit func(cell) bool) bool {
	page := db.page(pg)
	if page == nil || depth > 32 {
		return true
	}
	hdr := 0
	if pg == 1 {
		hdr = 100
	}
	if len(page) < hdr+12 {
		return true
	}
	n := int(binary.BigEndian.Uint16(page[hdr+3 : hdr+5]))
	switch page[hdr] {
	case 0x0d:
		for i := 0; i < n; i++ {
			po := hdr + 8 + 2*i
			if po+2 > len(page) {
				return true
			}
			off := int(binary.BigEndian.Uint16(page[po : po+2]))
			if off <= 0 || off >= len(page) {
				continue
			}
			p, n1 := varint(page[off:])
			rowid, n2 := varint(page[off+n1:])
			start := off + n1 + n2
			total := int(p)
			local, ovfl := total, 0
			x := db.usable - 35
			if total > x {
				m := (db.usable-12)*32/255 - 23
				k := m + (total-m)%(db.usable-4)
				if k <= x {
					local = k
				} else {
					local = m
				}
				if start+local+4 > len(page) {
					continue
				}
				ovfl = int(binary.BigEndian.Uint32(page[start+local : start+local+4]))
			}
			if start+local > len(page) {
				continue
			}
			if !visit(cell{rowid: rowid, total: total, local: page[start : start+local], ovfl: ovfl}) {
				return false
			}
		}
	case 0x05:
		for i := 0; i < n; i++ {
			po := hdr + 12 + 2*i
			if po+2 > len(page) {
				return true
			}
			off := int(binary.BigEndian.Uint16(page[po : po+2]))
			if off <= 0 || off+4 > len(page) {
				continue
			}
			if !db.walk(int(binary.BigEndian.Uint32(page[off:off+4])), depth+1, visit) {
				return false
			}
		}
		return db.walk(int(binary.BigEndian.Uint32(page[hdr+8:hdr+12])), depth+1, visit)
	}
	return true
}

// payload 拼接溢出页得到完整记录体。
func (db *sqliteDB) payload(c cell) []byte {
	buf := make([]byte, 0, c.total)
	buf = append(buf, c.local...)
	next := c.ovfl
	for len(buf) < c.total && next != 0 {
		pg := db.page(next)
		if pg == nil {
			break
		}
		next = int(binary.BigEndian.Uint32(pg[:4]))
		take := c.total - len(buf)
		if take > db.usable-4 {
			take = db.usable - 4
		}
		buf = append(buf, pg[4:4+take]...)
	}
	return buf
}

// decodeRecord 解码记录前 maxCols 列（缓冲不足则提前结束）。
func decodeRecord(b []byte, maxCols int) []any {
	hsz, n := varint(b)
	if n == 0 || int(hsz) > len(b) {
		return nil
	}
	var types []int64
	for p := n; p < int(hsz); {
		t, k := varint(b[p:])
		if k == 0 {
			break
		}
		types = append(types, t)
		p += k
	}
	body := b[hsz:]
	var out []any
	pos := 0
	for i, t := range types {
		if i >= maxCols {
			break
		}
		size := 0
		switch {
		case t == 0, t == 8, t == 9:
			size = 0
		case t >= 1 && t <= 4:
			size = int(t)
		case t == 5:
			size = 6
		case t == 6, t == 7:
			size = 8
		case t >= 12 && t%2 == 0:
			size = int(t-12) / 2
		case t >= 13:
			size = int(t-13) / 2
		}
		if pos+size > len(body) {
			break
		}
		raw := body[pos : pos+size]
		pos += size
		switch {
		case t == 0:
			out = append(out, nil)
		case t == 8:
			out = append(out, int64(0))
		case t == 9:
			out = append(out, int64(1))
		case t >= 1 && t <= 6:
			var v int64
			for _, bt := range raw {
				v = v<<8 | int64(bt)
			}
			if len(raw) > 0 && raw[0]&0x80 != 0 { // 符号扩展
				v -= int64(1) << (8 * uint(len(raw)))
			}
			out = append(out, v)
		case t == 7:
			out = append(out, float64(0))
		case t >= 13 && t%2 == 1:
			out = append(out, string(raw))
		default:
			out = append(out, append([]byte(nil), raw...))
		}
	}
	return out
}

// tableRoot 在 sqlite_master 中查表的根页号。
func (db *sqliteDB) tableRoot(name string) int {
	root := 0
	db.walk(1, 0, func(c cell) bool {
		cols := decodeRecord(db.payload(c), 5)
		if len(cols) >= 4 {
			if t, _ := cols[0].(string); t == "table" {
				if nm, _ := cols[1].(string); nm == name {
					if r, ok := cols[3].(int64); ok {
						root = int(r)
						return false
					}
				}
			}
		}
		return true
	})
	return root
}

// itemTable 返回 ItemTable 中 key 满足 match 的全部 (key, value)。
func (db *sqliteDB) itemTable(match func(key string) bool) (map[string][]byte, error) {
	root := db.tableRoot("ItemTable")
	if root == 0 {
		return nil, errors.New("未找到 ItemTable")
	}
	out := map[string][]byte{}
	db.walk(root, 0, func(c cell) bool {
		head := decodeRecord(c.local, 1)
		if len(head) < 1 {
			return true
		}
		key, _ := head[0].(string)
		if key == "" || !match(key) {
			return true
		}
		cols := decodeRecord(db.payload(c), 2)
		if len(cols) < 2 {
			return true
		}
		switch v := cols[1].(type) {
		case string:
			out[key] = []byte(v)
		case []byte:
			out[key] = v
		}
		return true
	})
	return out, nil
}
