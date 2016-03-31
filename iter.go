package tindex

import (
	"io"
	"sort"
)

type Iterator interface {
	// next retrieves the next document ID in the postings list.
	Next() (uint64, error)
	// seek moves the cursor to ID or the closest following one, if it doesn't exist.
	// It returns the ID at the position.
	Seek(id uint64) (uint64, error)
	// Close the iterator
	Close() error
}

type mergeIterator struct {
	i1, i2 Iterator
	v1, v2 uint64
	e1, e2 error
}

func (it *mergeIterator) Close() error {
	if err := it.i1.Close(); err != nil {
		return err
	}
	return it.i2.Close()
}

func (it *mergeIterator) Next() (uint64, error) {
	if it.e1 == io.EOF && it.e2 == io.EOF {
		return 0, io.EOF
	}
	if it.e1 != nil {
		if it.e1 != io.EOF {
			return 0, it.e1
		}
		x := it.v2
		it.v2, it.e2 = it.i2.Next()
		return x, nil
	}
	if it.e2 != nil {
		if it.e2 != io.EOF {
			return 0, it.e2
		}
		x := it.v1
		it.v1, it.e1 = it.i1.Next()
		return x, nil
	}
	if it.v1 < it.v2 {
		x := it.v1
		it.v1, it.e1 = it.i1.Next()
		return x, nil
	} else if it.v2 < it.v1 {
		x := it.v2
		it.v2, it.e2 = it.i2.Next()
		return x, nil
	} else {
		x := it.v1
		it.v1, it.e1 = it.i1.Next()
		it.v2, it.e2 = it.i2.Next()
		return x, nil
	}
}

func (it *mergeIterator) Seek(id uint64) (uint64, error) {
	// We just have to advance the first iterator. The next common match is also
	// the next seeked ID of the intersection.
	it.v1, it.e1 = it.i1.Seek(id)
	it.v2, it.e2 = it.i2.Seek(id)
	return it.Next()
}

func Merge(its ...Iterator) Iterator {
	if len(its) == 0 {
		return nil
	}
	i1 := its[0]

	for _, i2 := range its[1:] {
		i1 = &mergeIterator{i1: i1, i2: i2}
	}
	return i1
}

func ExpandIterator(it Iterator) ([]uint64, error) {
	var (
		res = []uint64{}
		v   uint64
		err error
	)
	for v, err = it.Seek(0); err == nil; v, err = it.Next() {
		res = append(res, v)
	}
	if err == io.EOF {
		err = nil
	}
	return res, err
}

type intersectIterator struct {
	i1, i2 Iterator
	v1, v2 uint64
	e1, e2 error
}

func (it *intersectIterator) Close() error {
	if err := it.i1.Close(); err != nil {
		return err
	}
	return it.i2.Close()
}

func Intersect(its ...Iterator) Iterator {
	if len(its) == 0 {
		return nil
	}
	i1 := its[0]

	for _, i2 := range its[1:] {
		i1 = &intersectIterator{i1: i1, i2: i2}
	}
	return i1
}

func (it *intersectIterator) Next() (uint64, error) {
	for {
		if it.e1 != nil {
			return 0, it.e1
		}
		if it.e2 != nil {
			return 0, it.e2
		}
		if it.v1 < it.v2 {
			it.v1, it.e1 = it.i1.Seek(it.v2)
		} else if it.v2 < it.v1 {
			it.v2, it.e2 = it.i2.Seek(it.v1)
		} else {
			v := it.v1
			it.v1, it.e1 = it.i1.Next()
			it.v2, it.e2 = it.i2.Next()
			return v, nil
		}
	}
}

func (it *intersectIterator) Seek(id uint64) (uint64, error) {
	// We have to advance both iterators. Otherwise, we get a false-positive
	// match on 0 if only on of the iterators has it.
	it.v1, it.e1 = it.i1.Seek(id)
	it.v2, it.e2 = it.i2.Seek(id)
	return it.Next()
}

// A skiplist iterator iterates through a list of value/pointer pairs.
type skiplistIterator interface {
	// seek returns the value and pointer at or before v.
	seek(v uint64) (val, ptr uint64, err error)
	// next returns the next value/pointer pair.
	next() (val, ptr uint64, err error)
}

// type skiplistCursor interface {
// 	skiplistIterator
// 	// Append the document ID d with pointer p to the skiplist.
// 	// Returns an error if d is not strictly greater than the last ID.
// 	append(d, p uint64) error
// }

// iteratorStore allows to retrieve an iterator based on a key.
type iteratorStore interface {
	get(uint64) (Iterator, error)
}

// skippingIterator implements the iterator interface based on skiplist, which
// allows to jump to the iterator closest to the seeked value.
//
// This iterator allows for speed up in seeks if the underlying data cannot
// be searched in O(log n).
// Ideally, the skiplist is seekable in O(log n).
type skippingIterator struct {
	skiplist  skiplistIterator
	iterators iteratorStore
	close     func() error

	// The iterator holding the next value.
	cur Iterator
}

// Close implements the Iterator interface.
func (it *skippingIterator) Close() error {
	if it.close == nil {
		return nil
	}
	return it.close()
}

// Seek implements the Iterator interface.
func (it *skippingIterator) Seek(id uint64) (uint64, error) {
	_, ptr, err := it.skiplist.seek(id)
	if err != nil {
		return 0, err
	}
	cur, err := it.iterators.get(ptr)
	if err != nil {
		return 0, err
	}
	it.cur = cur

	return it.cur.Seek(id)
}

// Next implements the Iterator interface.
func (it *skippingIterator) Next() (uint64, error) {
	// If next was called initially.
	// TODO(fabxc): should this just panic and initial call to seek() be required?
	if it.cur == nil {
		return it.Seek(0)
	}

	if id, err := it.cur.Next(); err == nil {
		return id, nil
	} else if err != io.EOF {
		return 0, err
	}
	// We reached the end of the current iterator. Get the next iterator through
	// our skiplist.
	_, ptr, err := it.skiplist.next()
	if err != nil {
		// Here we return the actual io.EOF if we reached the end of the iterator
		// retrieved from the last skiplist entry.
		return 0, err
	}
	// Iterate over the next iterator.
	cur, err := it.iterators.get(ptr)
	if err != nil {
		return 0, err
	}
	it.cur = cur

	// Return the first value in the new iterator.
	return it.cur.Seek(0)
}

// plainListIterator implements the iterator interface on a sorted list of integers.
type plainListIterator struct {
	list list
	pos  int
}

func newPlainListIterator(l []uint64) *plainListIterator {
	it := &plainListIterator{list: list(l)}
	sort.Sort(it.list)
	return it
}

func (it *plainListIterator) Close() error {
	return nil
}

func (it *plainListIterator) Seek(id uint64) (uint64, error) {
	it.pos = sort.Search(it.list.Len(), func(i int) bool { return it.list[i] >= id })
	return it.Next()

}

func (it *plainListIterator) Next() (uint64, error) {
	if it.pos >= it.list.Len() {
		return 0, io.EOF
	}
	x := it.list[it.pos]
	it.pos++
	return x, nil
}

type list []uint64

func (l list) Len() int           { return len(l) }
func (l list) Less(i, j int) bool { return l[i] < l[j] }
func (l list) Swap(i, j int)      { l[i], l[j] = l[j], l[i] }

// plainSkiplistIterator implements the skiplistIterator interface on plain
// in-memory mapping.
type plainSkiplistIterator struct {
	m    map[uint64]uint64
	keys list
	pos  int
}

func newPlainSkiplistIterator(m map[uint64]uint64) *plainSkiplistIterator {
	var keys list
	for k := range m {
		keys = append(keys, k)
	}
	sort.Sort(keys)

	return &plainSkiplistIterator{
		m:    m,
		keys: keys,
	}
}

func (it *plainSkiplistIterator) Close() error {
	return nil
}

// seek implements the skiplistIterator interface.
func (it *plainSkiplistIterator) seek(id uint64) (uint64, uint64, error) {
	pos := sort.Search(len(it.keys), func(i int) bool { return it.keys[i] >= id })
	// The skiplist iterator points to the element at or before the last value.
	if pos > 0 && it.keys[pos] > id {
		it.pos = pos - 1
	} else {
		it.pos = pos
	}
	return it.next()

}

// next implements the skiplistIterator interface.
func (it *plainSkiplistIterator) next() (uint64, uint64, error) {
	if it.pos >= len(it.keys) {
		return 0, 0, io.EOF
	}
	k := it.keys[it.pos]
	it.pos++
	return k, it.m[k], nil
}
