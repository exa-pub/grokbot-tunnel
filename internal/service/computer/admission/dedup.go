package admission

import "slices"

// dedupWindow is how many IDs a dedup remembers.
const dedupWindow = 4096

// dedup remembers the most recent IDs.
type dedup struct {
	seen  map[string]bool
	order []string
}

func newDedup() dedup { return dedup{seen: map[string]bool{}} }

func (d *dedup) has(id string) bool { return d.seen[id] }

// add reports whether id is new.
func (d *dedup) add(id string) bool {
	if d.seen[id] {
		return false
	}
	d.seen[id] = true
	d.order = append(d.order, id)
	if len(d.order) > dedupWindow {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return true
}

// remove forgets id.
func (d *dedup) remove(id string) {
	if !d.seen[id] {
		return
	}
	delete(d.seen, id)
	d.order = slices.DeleteFunc(d.order, func(seen string) bool { return seen == id })
}
