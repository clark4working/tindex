package tindex

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/boltdb/bolt"
)

var (
	bucketLabelToID  = []byte("label_to_id")
	bucketIDToLabel  = []byte("id_to_label")
	bucketSeriesToID = []byte("series_to_id")
	bucketIDToSeries = []byte("id_to_series")

	bucketPostings = []byte("postings")
	bucketSkiplist = []byte("skiplist")
)

func init() {
	if _, ok := seriesStores["bolt"]; ok {
		panic("bolt series store initialized twice")
	}
	seriesStores["bolt"] = newBoltSeriesStore

	if _, ok := postingsStores["bolt"]; ok {
		panic("bolt postings store initialized twice")
	}
	postingsStores["bolt"] = newBoltPostingsStore
}

type boltPostingsStore struct {
	db *bolt.DB
}

func newBoltPostingsStore(path string) (postingsStore, error) {
	if err := os.MkdirAll(path, 0777); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(path, "postings.db"), 0666, nil)
	if err != nil {
		return nil, err
	}
	s := &boltPostingsStore{
		db: db,
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err = tx.CreateBucketIfNotExists(bucketPostings); err != nil {
			return err
		}
		if _, err = tx.CreateBucketIfNotExists(bucketSkiplist); err != nil {
			return err
		}
		return nil
	})
	return s, err
}

func (s *boltPostingsStore) Close() error {
	return s.db.Close()
}

func (s *boltPostingsStore) Begin(writeable bool) (postingsTx, error) {
	tx, err := s.db.Begin(writeable)
	if err != nil {
		return nil, err
	}
	return &boltPostingsTx{
		Tx:       tx,
		skiplist: tx.Bucket(bucketSkiplist),
		postings: tx.Bucket(bucketPostings),
	}, nil
}

type boltPostingsTx struct {
	*bolt.Tx

	skiplist *bolt.Bucket
	postings *bolt.Bucket
}

type iteratorStoreFunc func(k uint64) (iterator, error)

func (s iteratorStoreFunc) get(k uint64) (iterator, error) {
	return s(k)
}

func (p *boltPostingsTx) iter(k uint64) (iterator, error) {
	b := p.skiplist.Bucket(encodeUint64(k))
	if b == nil {
		return nil, errNotFound
	}

	it := &skipIterator{
		skiplist: &boltSkiplistCursor{
			k:   k,
			c:   b.Cursor(),
			bkt: b,
		},
		iterators: iteratorStoreFunc(func(k uint64) (iterator, error) {
			data := p.postings.Get(encodeUint64(k))
			if data == nil {
				return nil, errNotFound
			}
			// TODO(fabxc): for now, offset is zero, pages have no header
			// and are always delta encoded.
			return newPageDelta(data).cursor(), nil
		}),
	}
	return it, nil
}

func (p *boltPostingsTx) append(k, id uint64) error {
	b, err := p.skiplist.CreateBucketIfNotExists(encodeUint64(k))
	if err != nil {
		return err
	}
	sl := &boltSkiplistCursor{
		k: k,
		c: b.Cursor(),
	}
	_, pid, err := sl.seek(math.MaxUint64)
	if err != nil {
		if err == errNotFound {
			pid, err = p.postings.NextSequence()
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	var pg page
	pdata := p.postings.Get(encodeUint64(pid))
	if pdata == nil {
		// The page ID was newly allocated but the page doesn't exist yet.
		pg = newPageDelta(make([]byte, pageSize))
		if err := pg.init(id); err != nil {
			return err
		}
	} else {
		pg = newPageDelta(pdata)

		if err := pg.cursor().append(id); err != errPageFull {
			return err
		} else {
			// We couldn't append to the page because it was full.
			// Allocate a new page.
			pid, err = p.postings.NextSequence()
			if err != nil {
				return err
			}
			pg = newPageDelta(make([]byte, pageSize))
			if err := pg.init(id); err != nil {
				return err
			}
		}
	}

	// Update the page in Bolt.
	return p.postings.Put(encodeUint64(pid), pg.data())
}

// boltSkiplistCursor implements the skiplistCurosr interface.
//
// TODO(fabxc): benchmark the overhead of a bucket per key.
// It might be more performant to have all skiplists in the same bucket.
//
// 	20k keys, ~10 skiplist entries avg -> 200k keys, 1 bucket vs 20k buckets, 10 keys
//
type boltSkiplistCursor struct {
	// k is currently unused. If the bucket holds entries for more than
	// just a single key, it will be necessary.
	k   uint64
	c   *bolt.Cursor
	bkt *bolt.Bucket
}

func (s *boltSkiplistCursor) next() (uint64, uint64, error) {
	db, pb := s.c.Next()
	if db == nil {
		return 0, 0, errNotFound
	}
	return decodeUint64(db), decodeUint64(pb), nil
}

func (s *boltSkiplistCursor) seek(k uint64) (uint64, uint64, error) {
	db, pb := s.c.Seek(encodeUint64(k))
	if db == nil {
		db, pb = s.c.Last()
		if db == nil {
			return 0, 0, errNotFound
		}
	}
	did, pid := decodeUint64(db), decodeUint64(pb)

	if did > k {
		db, pb = s.c.Prev()
		if db == nil {
			return 0, 0, errNotFound
		}
		did, pid = decodeUint64(db), decodeUint64(pb)
	}
	return did, pid, nil
}

func (s *boltSkiplistCursor) append(d, p uint64) error {
	k, _ := s.c.Last()

	if k != nil && decodeUint64(k) >= uint64(d) {
		return errOutOfOrder
	}

	return s.bkt.Put(encodeUint64(p), encodeUint64(p))
}

// boltSeriesStore implements a seriesStore based on Bolt.
type boltSeriesStore struct {
	db *bolt.DB
}

// newBoltSeriesStore initializes a Bolt-based seriesStore under path.
func newBoltSeriesStore(path string) (seriesStore, error) {
	if err := os.MkdirAll(path, 0777); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(path, "series.db"), 0666, nil)
	if err != nil {
		return nil, err
	}
	s := &boltSeriesStore{
		db: db,
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err = tx.CreateBucketIfNotExists(bucketLabelToID); err != nil {
			return err
		}
		if _, err = tx.CreateBucketIfNotExists(bucketIDToLabel); err != nil {
			return err
		}
		if _, err = tx.CreateBucketIfNotExists(bucketSeriesToID); err != nil {
			return err
		}
		if _, err = tx.CreateBucketIfNotExists(bucketIDToSeries); err != nil {
			return err
		}
		return nil
	})
	return s, err
}

func (s *boltSeriesStore) Close() error {
	return s.db.Close()
}

// Begin implements the seriesStore interface.
func (s *boltSeriesStore) Begin(writeable bool) (seriesTx, error) {
	tx, err := s.db.Begin(writeable)
	if err != nil {
		return nil, err
	}
	return &boltSeriesTx{
		Tx:         tx,
		IDToSeries: tx.Bucket(bucketIDToSeries),
		seriesToID: tx.Bucket(bucketSeriesToID),
		IDToLabel:  tx.Bucket(bucketIDToLabel),
		labelToID:  tx.Bucket(bucketLabelToID),
	}, nil
}

// boltSeriesTx implements a seriesTx.
type boltSeriesTx struct {
	*bolt.Tx

	seriesToID, IDToSeries *bolt.Bucket
	labelToID, IDToLabel   *bolt.Bucket
}

// series implements the seriesTx interface.
func (s *boltSeriesTx) series(sid uint64) (map[string]string, error) {
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, sid)

	series := s.IDToSeries.Get(buf[:n])
	if series == nil {
		return nil, errNotFound
	}
	var ids []uint64
	r := bytes.NewReader(series)
	for r.Len() > 0 {
		id, _, err := readUvarint(r)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	m := map[string]string{}

	for _, id := range ids {
		k, v, err := s.label(id)
		if err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, nil
}

// ensureSeries implements the seriesTx interface.
func (s *boltSeriesTx) ensureSeries(desc map[string]string) (uint64, seriesKey, error) {
	// Ensure that all labels are persisted.
	var skey seriesKey
	for k, v := range desc {
		key, err := s.ensureLabel(k, v)
		if err != nil {
			return 0, nil, err
		}
		skey = append(skey, key)
	}
	sort.Sort(skey)

	// Check whether we have seen the series before.
	var sid uint64
	if sidb := s.IDToSeries.Get(skey.bytes()); sidb != nil {
		sid, _ := binary.Uvarint(sidb)
		// TODO(fabxc): validate.
		return sid, skey, nil
	}

	// We haven't seen this series before, create a new ID.
	sid, err := s.IDToSeries.NextSequence()
	if err != nil {
		return 0, nil, err
	}
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, sid)

	if err := s.IDToSeries.Put(buf[:n], skey.bytes()); err != nil {
		return 0, nil, err
	}

	return sid, skey, nil
}

// label retrieves the key/value label associated with id.
func (s *boltSeriesTx) label(id uint64) (string, string, error) {
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, id)
	label := s.IDToLabel.Get(buf[:n])
	if label == nil {
		return "", "", fmt.Errorf("label for ID %q not found", buf[:n])
	}
	p := bytes.Split(label, []byte{seperator})

	return string(p[0]), string(p[1]), nil
}

// ensureLabel returns a unique ID for the label. If the label was not seen
// before a new, monotonically increasing ID is returned.
func (s *boltSeriesTx) ensureLabel(key, val string) (uint64, error) {
	k := make([]byte, len(key)+len(val)+1)

	copy(k[:len(key)], []byte(val))
	k[len(key)] = seperator
	copy(k[len(key)+1:], []byte(val))

	var err error
	var id uint64
	if v := s.labelToID.Get(k); v != nil {
		id, _ = binary.Uvarint(v)
	} else {
		id, err = s.IDToLabel.NextSequence()
		if err != nil {
			return 0, err
		}
		buf := make([]byte, binary.MaxVarintLen64)
		n := binary.PutUvarint(buf, id)
		if err := s.labelToID.Put(k, buf[:n]); err != nil {
			return 0, err
		}
		if err := s.IDToLabel.Put(buf[:n], k); err != nil {
			return 0, err
		}
	}

	return id, nil
}

func (s *boltSeriesTx) labels(m Matcher) (ids []uint64, err error) {
	c := s.labelToID.Cursor()

	for lbl, id := c.Seek([]byte(m.Key())); bytes.HasPrefix(lbl, []byte(m.Key())); lbl, id = c.Next() {
		p := bytes.Split(lbl, []byte{seperator})
		if !m.Match(string(p[1])) {
			continue
		}
		ids = append(ids, binary.BigEndian.Uint64(id))
	}

	return ids, nil
}
