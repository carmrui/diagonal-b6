package compact

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"diagonal.works/b6"
	"diagonal.works/b6/encoding"
	"diagonal.works/b6/ingest"
	"diagonal.works/b6/osm"
	pb "diagonal.works/b6/proto"

	"github.com/golang/geo/s2"
	"golang.org/x/exp/mmap"
)

type WriteCloserAt interface {
	io.WriterAt
	io.Closer
}

type ReadCloserAt interface {
	io.ReaderAt
	io.Closer
}

type ReadWriteCloserAt interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
}

type Output interface {
	Write() (WriteCloserAt, error)
	Read() (ReadCloserAt, error)
	ReadWrite() (ReadWriteCloserAt, error)
	Bytes() ([]byte, io.Closer, error)
}

type FileOutput string

func (f FileOutput) Read() (ReadCloserAt, error) {
	r, err := mmap.Open(string(f))
	if err != nil {
		return nil, fmt.Errorf("Failed to mmap %s: %w", f, err)

	}
	return r, nil
}

func (f FileOutput) Write() (WriteCloserAt, error) {
	w, err := os.OpenFile(string(f), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("Failed to open %s for write: %w", f, err)
	}
	return w, nil
}

func (f FileOutput) ReadWrite() (ReadWriteCloserAt, error) {
	w, err := os.OpenFile(string(f), os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("Failed to open %s for write: %w", f, err)
	}
	return w, nil
}

func (f FileOutput) Bytes() ([]byte, io.Closer, error) {
	m, err := encoding.Mmap(string(f))
	return m.Data, m, err
}

type MemoryOutput struct {
	encoding.Buffer
}

func (o *MemoryOutput) Write() (WriteCloserAt, error) {
	return &o.Buffer, nil
}

func (o *MemoryOutput) Read() (ReadCloserAt, error) {
	return &o.Buffer, nil
}

func (o *MemoryOutput) ReadWrite() (ReadWriteCloserAt, error) {
	return &o.Buffer, nil
}

func (o *MemoryOutput) Bytes() ([]byte, io.Closer, error) {
	return o.Buffer.Bytes(), &o.Buffer, nil
}

type OutputType int

const (
	OutputTypeMemory OutputType = 0
	OutputTypeDisk              = 1
)

type Options struct {
	Cores                int
	WorkDirectory        string
	OutputFilename       string
	PointsWorkOutputType OutputType
}

func (o *Options) Output() Output {
	return FileOutput(o.OutputFilename)
}

func (o *Options) PointsWorkOutput() Output {
	if o.PointsWorkOutputType == OutputTypeMemory {
		return &MemoryOutput{}
	} else {
		return FileOutput(path.Join(o.WorkDirectory, "points.work"))
	}
}

const (
	maxEncodedFeatureSize = 1 << 19 // Measured empirically

	// Tags used for the first pass
	PointTag         encoding.Tag = 0
	PointPathTag                  = 1
	PointRelationTag              = 2

	// Tags used in the second pass
	PointTagCommon         encoding.Tag = 0
	PointTagFull                        = 1
	PointTagReferencesOnly              = 2
)

var tagBits = map[b6.FeatureType]int{
	b6.FeatureTypePoint:    2,
	b6.FeatureTypePath:     0,
	b6.FeatureTypeArea:     0,
	b6.FeatureTypeRelation: 0,
}

type toMap func(id FeatureID, tag encoding.Tag, buffer []byte) error

func emitPoints(source ingest.FeatureSource, o *Options, s *encoding.StringTableBuilder, nt *NamespaceTable, emit toMap) error {
	goroutines := o.Cores * 2
	buffers := make([][]byte, goroutines)
	for i := range buffers {
		buffers[i] = make([]byte, maxEncodedFeatureSize)
	}
	points := make([]Point, goroutines)
	var seen Counts
	emitFeature := func(feature ingest.Feature, g int) error {
		switch f := feature.(type) {
		case *ingest.PointFeature:
			if n := atomic.AddUint64(&seen.Points, 1); n%10000000 == 0 {
				log.Printf("  %d points", n)
			}
			points[g].FromFeature(f, s)
			n := points[g].Marshal(buffers[g])
			eid := FeatureID{Namespace: nt.Encode(f.PointID.Namespace), Type: b6.FeatureTypePoint, Value: f.PointID.Value}
			emit(eid, PointTag, buffers[g][0:n])
		case *ingest.PathFeature:
			if n := atomic.AddUint64(&seen.Paths, 1); n%1000000 == 0 {
				log.Printf("  %d paths", n)
			}
			r := Reference{Namespace: nt.Encode(f.PathID.Namespace), Value: f.PathID.Value}
			n := r.Marshal(nt.Encode(b6.NamespaceOSMWay), buffers[g])
			end := f.Len()
			if f.IsClosed() {
				end--
			}
			// Note that ways that reference a path may be dropped from the index at a
			// later stage (becuause, for example, they're missing points), so we need
			// to handle this at query time.
			for i := 0; i < end; i++ {
				if id, ok := f.PointID(i); ok {
					eid := FeatureID{Namespace: nt.Encode(id.Namespace), Type: b6.FeatureTypePoint, Value: id.Value}
					emit(eid, PointPathTag, buffers[g][0:n])
				}
			}
		case *ingest.RelationFeature:
			if n := atomic.AddUint64(&seen.Relations, 1); n%1000000 == 0 {
				log.Printf("  %d relations", n)
			}
			r := Reference{Namespace: nt.Encode(f.RelationID.Namespace), Value: f.RelationID.Value}
			n := r.Marshal(nt.Encode(b6.NamespaceOSMRelation), buffers[g])
			for _, m := range f.Members {
				if m.ID.Type == b6.FeatureTypePoint { // Non-point types handled via Summary
					eid := FeatureID{Namespace: nt.Encode(m.ID.Namespace), Type: m.ID.Type, Value: m.ID.Value}
					emit(eid, PointRelationTag, buffers[g][0:n])
				}
			}
		}
		return nil
	}

	options := ingest.ReadOptions{
		SkipPoints:    false,
		SkipTags:      false,
		SkipPaths:     false,
		SkipRelations: false,
		Cores:         goroutines,
	}
	if err := source.Read(options, emitFeature, context.Background()); err != nil {
		return err
	}
	log.Printf("emitPoints: %+v", seen)
	return nil
}

func bucketBitsForCount(count uint64) int {
	bits := int(math.Ceil(math.Log(float64(count)) / math.Log(2.0)))
	if bits <= 0 {
		bits = 1
	}
	return bits
}

func addFeatureBlockBuilder(builders FeatureBlockBuilders, t b6.FeatureType, ns b6.Namespace, count uint64, nt *NamespaceTable) {
	namespaces := OSMNamespaces(nt)
	namespaces[t] = nt.Encode(ns)
	log.Printf("addFeatureBlockBuilder: %s: %s %d", ns, t, count)
	key := NamespacedFeatureType{Namespace: nt.Encode(ns), FeatureType: t}
	builders[key] = &FeatureBlockBuilder{
		Header: FeatureBlockHeader{FeatureType: t, Namespaces: namespaces},
		Map:    encoding.NewUint64MapBuilder(bucketBitsForCount(count), tagBits[t]),
	}
}

func newFeatureBlockBuilders(nt *NamespaceTable, summary *Summary) FeatureBlockBuilders {
	builders := make(FeatureBlockBuilders)
	for ns, c := range summary.Counts.ByNamespace {
		// TODO: factor out by making counts two arrays indexed by FeatureType,
		// one for values and one for references
		var points uint64
		if c.Points > 0 {
			points = c.Points
		} else if c.PathPoints > 0 {
			points = c.PathPoints
		}
		if points > 0 {
			addFeatureBlockBuilder(builders, b6.FeatureTypePoint, ns, points, nt)
		}
		if c.Paths > 0 {
			addFeatureBlockBuilder(builders, b6.FeatureTypePath, ns, c.Paths, nt)
		}
		if c.Areas > 0 {
			addFeatureBlockBuilder(builders, b6.FeatureTypeArea, ns, c.Areas, nt)
		}
		if c.Relations > 0 {
			addFeatureBlockBuilder(builders, b6.FeatureTypeRelation, ns, c.Relations, nt)
		}
	}
	return builders
}

func writePointsWork(source ingest.FeatureSource, o *Options, strings *encoding.StringTableBuilder, nt *NamespaceTable, summary *Summary, w io.WriterAt) error {
	builders := newFeatureBlockBuilders(nt, summary)
	emit := func(id FeatureID, tag encoding.Tag, buffer []byte) error {
		builders.Reserve(id, tag, len(buffer))
		return nil
	}
	log.Printf("writePointsWork: reserve")
	if err := emitPoints(source, o, strings, nt, emit); err != nil {
		return err
	}

	log.Printf("writePointsWork: write header")
	if _, err := builders.WriteHeaders(w, 0); err != nil {
		return err
	}

	emit = func(id FeatureID, tag encoding.Tag, buffer []byte) error {
		return builders.WriteItem(id, tag, buffer, w)
	}
	log.Printf("writePointsWork: write entries")
	if err := emitPoints(source, o, strings, nt, emit); err != nil {
		return err
	}
	return nil
}

func combinePoints(points *encoding.Uint64Map, nss *Namespaces, goroutines int, emit func(id uint64, tag encoding.Tag, buffer []byte) error) error {
	if goroutines < 1 {
		goroutines = 1
	}
	buffers := make([][]byte, goroutines)
	for i := range buffers {
		buffers[i] = make([]byte, points.MaxBucketLength())
	}
	references := make([]PointReferences, goroutines)
	for i := range references {
		references[i].Paths = make(References, 64)     // Estimate
		references[i].Relations = make(References, 64) // Estimate
	}

	var noPoint, orphan, common, combines, totalTags uint64
	combine := func(id uint64, tags []encoding.Tagged, g int) error {
		atomic.AddUint64(&combines, 1)
		var point []byte
		references[g].Paths = references[g].Paths[0:0]
		references[g].Relations = references[g].Relations[0:0]
		atomic.AddUint64(&totalTags, uint64(len(tags)))
		for _, t := range tags {
			switch t.Tag {
			case PointTag:
				point = t.Data
			case PointPathTag:
				var r Reference
				r.Unmarshal(nss.ForType(b6.FeatureTypePath), t.Data)
				references[g].Paths = append(references[g].Paths, r)
			case PointRelationTag:
				var r Reference
				r.Unmarshal(nss.ForType(b6.FeatureTypeRelation), t.Data)
				references[g].Relations = append(references[g].Relations, r)
			default:
				panic(fmt.Sprintf("Unexpected tag: %d", t.Tag))
			}
		}
		if point != nil {
			if len(references[g].Paths) == 1 && len(references[g].Relations) == 0 {
				atomic.AddUint64(&common, 1)
				n := CombinePointAndPath(point, nss, references[g].Paths[0], buffers[g])
				emit(id, PointTagCommon, buffers[g][0:n])
			} else {
				if len(references[g].Paths) == 0 && len(references[g].Relations) == 0 {
					atomic.AddUint64(&orphan, 1)
				}
				n := CombinePointAndReferences(point, references[g], nss, buffers[g])
				emit(id, PointTagFull, buffers[g][0:n])
			}
		} else {
			atomic.AddUint64(&noPoint, 1)
			n := references[g].Marshal(nss, buffers[g])
			emit(id, PointTagReferencesOnly, buffers[g][0:n])
		}
		return nil
	}
	if err := points.EachItem(combine, goroutines); err != nil {
		return err
	}
	log.Printf("combinePoints: noPoint: %d orphan: %d common: %d combines: %d map tags: %d", noPoint, orphan, common, combines, totalTags)
	return nil
}

func writePoints(o *Options, points FeatureBlocks, strings *encoding.StringTableBuilder, nt *NamespaceTable, summary *Summary, offset encoding.Offset, w io.WriterAt) (encoding.Offset, error) {
	builders := newFeatureBlockBuilders(nt, summary)
	log.Printf("writePoints: reserve")
	var ns Namespace
	emit := func(value uint64, tag encoding.Tag, buffer []byte) error {
		id := FeatureID{Namespace: ns, Type: b6.FeatureTypePoint, Value: value}
		builders.Reserve(id, tag, len(buffer))
		return nil
	}
	osmNamespaces := OSMNamespaces(nt)
	for _, b := range points {
		ns = b.Namespaces[b6.FeatureTypePoint]
		if err := combinePoints(b.Map, &osmNamespaces, o.Cores, emit); err != nil {
			return 0, err
		}
	}

	log.Printf("writePoints: write header")
	var err error
	offset, err = builders.WriteHeaders(w, offset)
	if err != nil {
		return offset, err
	}

	log.Printf("writePoints: write entries")
	emit = func(value uint64, tag encoding.Tag, buffer []byte) error {
		id := FeatureID{Namespace: ns, Type: b6.FeatureTypePoint, Value: value}
		return builders.WriteItem(id, tag, buffer, w)
	}
	for _, b := range points {
		ns = b.Namespaces[b6.FeatureTypePoint]
		if err := combinePoints(b.Map, &osmNamespaces, o.Cores, emit); err != nil {
			return 0, err
		}
	}
	return offset, nil
}

func logPanic(f osm.EmitWithGoroutine) osm.EmitWithGoroutine {
	ff := func(e osm.Element, g int) error {
		defer func() {
			if r := recover(); r != nil {
				panic(fmt.Sprintf("on element %+v", e))
			}
		}()
		return f(e, g)
	}
	return ff
}

type overlayLocationsByID struct {
	overlay b6.LocationsByID
	base    b6.LocationsByID
}

func (o *overlayLocationsByID) FindLocationByID(id b6.PointID) (s2.LatLng, bool) {
	if ll, ok := o.overlay.FindLocationByID(id); ok {
		return ll, ok
	}
	return o.base.FindLocationByID(id)
}

type ValidationState int

const (
	ValidationStateValid ValidationState = iota
	ValidationStateInvalid
	ValidationStateUnknown
)

type Validator struct {
	locations b6.LocationsByID
	paths     map[b6.PathID]ValidationState
	queue     []*ingest.AreaFeature
	lock      sync.Mutex
}

func NewValidator(locations b6.LocationsByID) *Validator {
	return &Validator{
		locations: locations,
		paths:     make(map[b6.PathID]ValidationState),
		queue:     make([]*ingest.AreaFeature, 0, 2),
	}
}

func (v *Validator) ValidatePath(p *ingest.PathFeature, fs []ingest.Feature) []ingest.Feature {
	var state ValidationState
	o := ingest.ValidateOptions{InvertClockwisePaths: true}
	if err := ingest.ValidatePath(p, &o, v.locations); err == nil {
		fs = append(fs, p)
		state = ValidationStateValid
	} else {
		state = ValidationStateInvalid
		log.Printf("ValidatePath: drop invalid path: %s", err)
	}
	validateQueue := false
	v.lock.Lock()
	if _, ok := v.paths[p.PathID]; ok {
		validateQueue = true
	}
	v.paths[p.PathID] = state
	if validateQueue {
		fs = v.validateQueue(fs)
	}
	v.lock.Unlock()
	return fs
}

func (v *Validator) ValidateArea(a *ingest.AreaFeature, fs []ingest.Feature) []ingest.Feature {
	v.lock.Lock()
	switch v.validateArea(a) {
	case ValidationStateValid:
		fs = append(fs, a)
	case ValidationStateUnknown:
		v.queue = append(v.queue, a.CloneAreaFeature())
	}
	v.lock.Unlock()
	return fs
}

func (v *Validator) validateArea(a *ingest.AreaFeature) ValidationState {
	state := ValidationStateValid
	for i := 0; i < a.Len(); i++ {
		if ids, ok := a.PathIDs(i); ok {
			for _, id := range ids {
				if s, ok := v.paths[id]; ok {
					if s == ValidationStateInvalid {
						state = ValidationStateInvalid
					} else if s == ValidationStateUnknown && state == ValidationStateValid {
						state = ValidationStateUnknown
					}
				} else {
					v.paths[id] = ValidationStateUnknown
					if state == ValidationStateValid {
						state = ValidationStateUnknown
					}
				}
			}
		}
	}
	return state
}

func (v *Validator) validateQueue(fs []ingest.Feature) []ingest.Feature {
	read := 0
	write := 0
	for read < len(v.queue) {
		switch v.validateArea(v.queue[read]) {
		case ValidationStateValid:
			fs = append(fs, v.queue[read])
		case ValidationStateUnknown:
			if write != read {
				v.queue[write] = v.queue[read]
			}
			write++
		}
		read++
	}
	v.queue = v.queue[0:write]
	return fs
}

func emitPathsAreasAndRelations(source ingest.FeatureSource, o *Options, s *encoding.StringTableBuilder, nt *NamespaceTable, locations b6.LocationsByID, summary *Summary, emit toMap) error {
	goroutines := o.Cores * 2
	if goroutines == 0 {
		goroutines = 1
	}
	buffers := make([][]byte, goroutines)
	for i := range buffers {
		buffers[i] = make([]byte, maxEncodedFeatureSize)
	}
	paths := make([]Path, goroutines)
	areas := make([]Area, goroutines)
	relations := make([]Relation, goroutines)
	var seen Counts
	osmNamespaces := OSMNamespaces(nt)
	emitFeature := func(feature ingest.Feature, g int) error {
		switch f := feature.(type) {
		case *ingest.PathFeature:
			if n := atomic.AddUint64(&seen.Paths, 1); n%1000000 == 0 {
				log.Printf("  %d paths", n)
			}
			paths[g].FromFeature(f, s, nt)
			paths[g].Areas = summary.PathAreas.FillReferences(paths[g].Areas[0:0], f.FeatureID(), nt)
			paths[g].Relations = summary.RelationMembers.FillReferences(paths[g].Relations[0:0], f.FeatureID(), nt)
			n := paths[g].Marshal(&osmNamespaces, buffers[g])
			eid := FeatureID{Namespace: nt.Encode(f.PathID.Namespace), Type: b6.FeatureTypePath, Value: f.PathID.Value}
			emit(eid, encoding.NoTag, buffers[g][0:n])
		case *ingest.AreaFeature:
			if n := atomic.AddUint64(&seen.Areas, 1); n%1000000 == 0 {
				log.Printf("  %d areas", n)
			}
			areas[g].FromFeature(f, s, nt)
			areas[g].Relations = summary.RelationMembers.FillReferences(areas[g].Relations[0:0], f.FeatureID(), nt)
			n := areas[g].Marshal(&osmNamespaces, buffers[g])
			eid := FeatureID{Namespace: nt.Encode(f.AreaID.Namespace), Type: b6.FeatureTypeArea, Value: f.AreaID.Value}
			emit(eid, encoding.NoTag, buffers[g][0:n])
		case *ingest.RelationFeature:
			if n := atomic.AddUint64(&seen.Relations, 1); n%1000000 == 0 {
				log.Printf("  %d relations", n)
			}
			relations[g].FromFeature(f, s, nt)
			relations[g].Relations = summary.RelationMembers.FillReferences(relations[g].Relations[0:0], f.FeatureID(), nt)
			n := relations[g].Marshal(b6.FeatureTypePath, &osmNamespaces, buffers[g])
			eid := FeatureID{Namespace: nt.Encode(f.RelationID.Namespace), Type: b6.FeatureTypeRelation, Value: f.RelationID.Value}
			emit(eid, encoding.NoTag, buffers[g][0:n])
		}
		return nil
	}

	validator := NewValidator(locations)
	validated := make([][]ingest.Feature, goroutines)
	for i := range validated {
		validated[i] = make([]ingest.Feature, 0, 2)
	}
	validateFeature := func(feature ingest.Feature, g int) error {
		switch f := feature.(type) {
		case *ingest.PathFeature:
			for _, v := range validator.ValidatePath(f, validated[g][0:0]) {
				if err := emitFeature(v, g); err != nil {
					return err
				}
			}
		case *ingest.AreaFeature:
			for _, v := range validator.ValidateArea(f, validated[g][0:0]) {
				if err := emitFeature(v, g); err != nil {
					return err
				}
			}
		case *ingest.RelationFeature:
			return emitFeature(f, g)
		}
		return nil
	}

	options := ingest.ReadOptions{
		SkipPoints:    true,
		SkipPaths:     false,
		SkipRelations: false,
		SkipTags:      false,
		Cores:         goroutines,
	}
	if err := source.Read(options, validateFeature, context.Background()); err != nil {
		return err
	}
	return nil
}

func writePathsAreasAndRelations(source ingest.FeatureSource, o *Options, strings *encoding.StringTableBuilder, nt *NamespaceTable, locations b6.LocationsByID, summary *Summary, offset encoding.Offset, w io.WriterAt) (encoding.Offset, error) {
	builders := newFeatureBlockBuilders(nt, summary)
	emit := func(id FeatureID, tag encoding.Tag, buffer []byte) error {
		builders.Reserve(id, tag, len(buffer))
		return nil
	}
	log.Printf("writePathsAreasAndRelations: reserve")
	if err := emitPathsAreasAndRelations(source, o, strings, nt, locations, summary, emit); err != nil {
		return offset, err
	}

	log.Printf("writePathsAreasAndRelations: write header")
	offset, err := builders.WriteHeaders(w, offset)
	if err != nil {
		return offset, err
	}

	emit = func(id FeatureID, tag encoding.Tag, buffer []byte) error {
		return builders.WriteItem(id, tag, buffer, w)
	}
	log.Printf("writePathsAreasAndRelations: write entries")
	if err := emitPathsAreasAndRelations(source, o, strings, nt, locations, summary, emit); err != nil {
		return offset, err
	}
	return offset, nil
}

type OSMRelationship struct {
	ID       osm.AnyID
	Relation osm.RelationID
}

type OSMRelationships []OSMRelationship

func (r OSMRelationships) Len() int      { return len(r) }
func (r OSMRelationships) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r OSMRelationships) Less(i, j int) bool {
	if r[i].ID == r[j].ID {
		return r[i].Relation < r[j].Relation
	}
	return r[i].ID < r[j].ID
}

func (r OSMRelationships) Begin(id osm.AnyID) int {
	return sort.Search(len(r), func(i int) bool {
		return r[i].ID >= id
	})
}

type Relationship [2]b6.FeatureID

type Relationships []Relationship

func (r Relationships) Len() int      { return len(r) }
func (r Relationships) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r Relationships) Less(i, j int) bool {
	if r[i][0] == r[j][0] {
		return r[i][1].Less(r[j][1])
	}
	return r[i][0].Less(r[j][0])
}

func (r Relationships) FillReferences(rs References, id b6.FeatureID, nt *NamespaceTable) References {
	j := sort.Search(len(r), func(i int) bool {
		return !r[i][0].Less(id)
	})
	last := b6.FeatureIDInvalid
	for j < len(r) && r[j][0] == id {
		if r[j][1] != last {
			rs = append(rs, Reference{Namespace: nt.Encode(r[j][1].Namespace), Value: r[j][1].Value})
			last = r[j][1]
		}
		j++
	}
	return rs
}

type Counts struct {
	Points          uint64
	Paths           uint64
	Areas           uint64
	Relations       uint64
	PathPoints      uint64
	AreaPaths       uint64
	RelationMembers uint64
}

type NamespacedCounts struct {
	ByNamespace map[b6.Namespace]*Counts
	lock        sync.RWMutex
}

func NewNamespacedCounts() *NamespacedCounts {
	return &NamespacedCounts{ByNamespace: make(map[b6.Namespace]*Counts)}
}

func (n *NamespacedCounts) Namespace(ns b6.Namespace) *Counts {
	n.lock.RLock()
	if c, ok := n.ByNamespace[ns]; ok {
		n.lock.RUnlock()
		return c
	}
	n.lock.RUnlock()
	n.lock.Lock()
	c, ok := n.ByNamespace[ns]
	if !ok {
		c = &Counts{}
		n.ByNamespace[ns] = c
	}
	n.lock.Unlock()
	return c
}

type Summary struct {
	Counts          *NamespacedCounts
	ClosedPaths     map[b6.PathID]struct{}
	RelationMembers Relationships
	PathAreas       Relationships
}

func NewSummary() *Summary {
	return &Summary{
		Counts:          NewNamespacedCounts(),
		ClosedPaths:     make(map[b6.PathID]struct{}),
		RelationMembers: make(Relationships, 0, 128),
		PathAreas:       make(Relationships, 0, 128),
	}
}

func isWayArea(way *osm.Way) bool {
	return way.Nodes[0] == way.Nodes[len(way.Nodes)-1]
}

func isRelationMultiPolygon(relation *osm.Relation) bool {
	if t, ok := relation.Tag("type"); ok {
		return t == "multipolygon"
	}
	return false
}

func fillStringTableAndSummary(source ingest.FeatureSource, o *Options, strings *encoding.StringTableBuilder, summary *Summary) error {
	var relationshipsLock sync.Mutex
	var closedPathsLock sync.Mutex
	emit := func(feature ingest.Feature, _ int) error {
		for _, tag := range feature.AllTags() {
			strings.Add(tag.Key)
			strings.Add(tag.Value)
		}
		counts := summary.Counts.Namespace(feature.FeatureID().Namespace)
		switch f := feature.(type) {
		case *ingest.PointFeature:
			atomic.AddUint64(&counts.Points, 1)
		case *ingest.PathFeature:
			atomic.AddUint64(&counts.Paths, 1)
			for i := 0; i < f.Len(); i++ {
				if id, ok := f.PointID(i); ok {
					c := summary.Counts.Namespace(id.Namespace)
					atomic.AddUint64(&c.PathPoints, 1)
				}
			}
			if f.Len() > 2 && f.IsClosed() {
				closedPathsLock.Lock()
				summary.ClosedPaths[f.PathID] = struct{}{}
				closedPathsLock.Unlock()
			}
		case *ingest.AreaFeature:
			atomic.AddUint64(&counts.Areas, 1)
			for i := 0; i < f.Len(); i++ {
				if ids, ok := f.PathIDs(i); ok {
					relationshipsLock.Lock()
					for _, id := range ids {
						summary.PathAreas = append(summary.PathAreas, Relationship{id.FeatureID(), f.FeatureID()})
					}
					relationshipsLock.Unlock()
					for _, id := range ids {
						c := summary.Counts.Namespace(id.Namespace)
						atomic.AddUint64(&c.AreaPaths, 1)
					}
				}
			}
		case *ingest.RelationFeature:
			atomic.AddUint64(&counts.Relations, 1)
			for _, member := range f.Members {
				strings.Add(member.Role)
			}
			relationshipsLock.Lock()
			for _, member := range f.Members {
				if member.ID.Type != b6.FeatureTypePoint { // Points are handled separately
					summary.RelationMembers = append(summary.RelationMembers, Relationship{member.ID, f.FeatureID()})
				}
			}
			relationshipsLock.Unlock()
			for _, member := range f.Members {
				c := summary.Counts.Namespace(member.ID.Namespace)
				atomic.AddUint64(&c.AreaPaths, 1)
			}
		}
		return nil
	}

	options := ingest.ReadOptions{
		Cores: o.Cores,
	}
	if err := source.Read(options, emit, context.Background()); err != nil {
		return err
	}
	sort.Sort(summary.RelationMembers)
	sort.Sort(summary.PathAreas)
	log.Printf("fillStringTableAndSummary: strings: %d relationships: %d path areas: %d", strings.NumStrings(), len(summary.RelationMembers), len(summary.PathAreas))
	return nil
}

type LocationsByID struct {
	bs FeatureBlocks
	nt *NamespaceTable
}

func NewLocationsByID(bs FeatureBlocks, nt *NamespaceTable) *LocationsByID {
	return &LocationsByID{bs: bs, nt: nt}
}

func (l LocationsByID) FindLocationByID(id b6.PointID) (s2.LatLng, bool) {
	for _, b := range l.bs {
		ns := l.nt.Encode(id.Namespace)
		if b.Namespaces[b6.FeatureTypePoint] == ns {
			if p := b.Map.FindFirstWithTag(id.Value, PointTag); p != nil {
				return MarshalledPoint(p).Location(), true
			}
		}
	}
	return s2.LatLng{}, false
}

func fillNamespaceTableFromSummary(summary *Summary, nt *NamespaceTable) {
	nss := make([]b6.Namespace, 0, len(summary.Counts.ByNamespace))
	for ns := range summary.Counts.ByNamespace {
		nss = append(nss, ns)
	}
	// Include all OSM namespaces, even if they're not mentioned in the
	// data, as they're used as defaults even if they're always overriden.
	for _, ns := range b6.OSMNamespaces {
		if _, ok := summary.Counts.ByNamespace[ns]; !ok {
			nss = append(nss, ns)
		}
	}
	nt.FillFromNamespaces(nss)
}

func buildFeatures(source ingest.FeatureSource, o *Options, sb *encoding.StringTableBuilder, nt *NamespaceTable, summary *Summary, header *Header, base b6.LocationsByID, w io.WriterAt) (encoding.Offset, error) {
	work := o.PointsWorkOutput()
	workW, err := work.Write()
	if err != nil {
		return 0, err
	}

	log.Printf("buildFeatures: points")
	err = writePointsWork(source, o, sb, nt, summary, workW)
	if err != nil {
		return 0, err
	}
	if err := workW.Close(); err != nil {
		return 0, err
	}

	data, closer, err := work.Bytes()
	if err != nil {
		return 0, err
	}
	points := make(FeatureBlocks, 0)
	points.Unmarshal(data)

	offset, err := writePoints(o, points, sb, nt, summary, header.BlockOffset, w)
	log.Printf("writePoints: %d", offset)
	if err != nil {
		return 0, err
	}

	locations := overlayLocationsByID{overlay: NewLocationsByID(points, nt), base: base}
	offset, err = writePathsAreasAndRelations(source, o, sb, nt, &locations, summary, offset, w)
	log.Printf("writePathsAreasAndRelations: %d", offset)
	if err != nil {
		return 0, err
	}
	return offset, closer.Close()
}

func fillIndex(byID *FeaturesByID, nt *NamespaceTable, index map[string]*FeatureIDs) ([]string, error) {
	allTokens := make([]string, 0)
	var lock sync.RWMutex
	emit := func(feature b6.Feature, goroutine int) error {
		tokens := ingest.TokensForFeature(feature.(b6.PhysicalFeature))
		for _, token := range tokens {
			var ids *FeatureIDs
			var ok bool
			lock.RLock()
			if ids, ok = index[token]; !ok {
				lock.RUnlock()
				lock.Lock()
				if ids, ok = index[token]; !ok {
					ids = &FeatureIDs{}
					index[token] = ids
					allTokens = append(allTokens, token)
				}
				lock.Unlock()
			} else {
				lock.RUnlock()
			}
			ids.Append(EncodeFeatureID(feature.FeatureID(), nt))
		}
		return nil
	}
	options := b6.EachFeatureOptions{Cores: runtime.NumCPU()}
	err := byID.EachFeature(emit, &options)
	return allTokens, err
}

func sortIndexIDs(index map[string]*FeatureIDs) {
	toSort := make(chan *FeatureIDs, runtime.NumCPU())
	var wg sync.WaitGroup
	finish := func() {
		for ids := range toSort {
			sort.Sort(ids)
		}
		wg.Done()
	}
	wg.Add(runtime.NumCPU())
	for i := 0; i < runtime.NumCPU(); i++ {
		go finish()
	}
	for _, ids := range index {
		toSort <- ids
	}
	close(toSort)
	wg.Wait()
}

func writeIndex(w io.WriterAt, offset encoding.Offset, tokens *TokenMapEncoder, lists *encoding.ByteArraysBuilder) (encoding.Offset, error) {
	var buffer [BlockHeaderLength]byte
	header := BlockHeader{
		Type:   BlockTypeSearchIndex,
		Length: uint64(tokens.Length() + lists.Length()),
	}
	l := header.Marshal(buffer[0:])
	if _, err := w.WriteAt(buffer[0:l], int64(offset)); err != nil {
		return 0, err
	}
	offset = offset.Add(l)
	offset, err := tokens.Write(w, offset)
	if err == nil {
		offset, err = lists.WriteHeader(w, offset)
	}
	return offset, err
}

func buildIndex(byID *FeaturesByID, nt *NamespaceTable, offset encoding.Offset, output Output) error {
	log.Printf("buildIndex: add")
	index := make(map[string]*FeatureIDs)
	allTokens, err := fillIndex(byID, nt, index)
	if err != nil {
		return err
	}
	log.Printf("buildIndex: sort")
	sort.Strings(allTokens)
	sortIndexIDs(index)

	o, err := output.ReadWrite()
	if err != nil {
		return err
	}

	tokens := NewTokenMapEncoder()
	for i, token := range allTokens {
		tokens.Add(token, i)
	}
	tokens.FinishAdds()
	lists := encoding.NewByteArraysBuilder(len(index))

	stages := []func(token int, header []byte, ids []byte) error{
		func(token int, header []byte, ids []byte) error {
			lists.Reserve(token, len(header))
			lists.Reserve(token, len(ids))
			return nil
		},
		func(token int, header []byte, ids []byte) error {
			return lists.WriteItem(o, token, header, ids)
		},
	}

	var pl PostingList
	pl.IDs = make([]byte, 0, 2048)
	buffer := make([]byte, PostingListHeaderMaxLength)
	for i, stage := range stages {
		if i == 1 {
			lists.FinishReservation()
			if _, err := writeIndex(o, offset, tokens, lists); err != nil {
				return err
			}
		}
		for j, token := range allTokens {
			pl.Fill(token, index[token].Begin())
			n := pl.Header.Marshal(buffer)
			if err := stage(j, buffer[0:n], pl.IDs); err != nil {
				return err
			}
		}
	}

	log.Printf("buildIndex: %d tokens", len(allTokens))
	return o.Close()
}

func summarise(source ingest.FeatureSource, o *Options) (*Summary, *encoding.StringTableBuilder, error) {
	log.Printf("summarise: build strings and summary")
	summary := NewSummary()
	sb := encoding.NewStringTableBuilder()
	if err := fillStringTableAndSummary(source, o, sb, summary); err != nil {
		return nil, nil, fmt.Errorf("Failed to build string table: %w", err)
	}
	return summary, sb, nil
}

func build(source ingest.FeatureSource, base b6.FeaturesByID, o *Options, output Output) error {
	var header Header
	header.Magic = HeaderMagic

	w, err := output.Write()
	if err != nil {
		return err
	}

	var buffer [HeaderLength]byte
	header.VersionOffset = encoding.Offset(header.Marshal(buffer[0:]))
	n := MarshalString(Version, buffer[0:])
	if n, err := w.WriteAt(buffer[0:n], int64(header.VersionOffset)); err == nil {
		header.HeaderProtoOffset = header.VersionOffset.Add(n)
	} else {
		return err
	}

	summary, sb, err := summarise(source, o)
	if err != nil {
		return err
	}

	var nt NamespaceTable
	fillNamespaceTableFromSummary(summary, &nt)

	var hp pb.CompactHeaderProto
	nt.FillProto(&hp)
	header.StringsOffset, err = WriteProto(w, &hp, header.HeaderProtoOffset)

	log.Printf("build: write strings")
	header.BlockOffset, err = sb.Write(w, header.StringsOffset)

	indexOffset, err := buildFeatures(source, o, sb, &nt, summary, &header, base, w)
	if err != nil {
		return err
	}

	n = header.Marshal(buffer[0:])
	if _, err := w.WriteAt(buffer[0:n], 0); err != nil {
		return err
	}
	w.Close()

	data, closer, err := output.Bytes()
	if err != nil {
		return err
	}
	defer closer.Close()
	byID, err := NewFeaturesByIDFromData(data, base)
	if err != nil {
		return fmt.Errorf("Failed to create FeaturesByID: %s", err)
	}

	byID.LogSummary()

	return buildIndex(byID, &nt, indexOffset, output)
}

func Build(source ingest.FeatureSource, o *Options) error {
	return build(source, emptyFeaturesByID{}, o, o.Output())
}

func BuildInMemory(source ingest.FeatureSource, o *Options) ([]byte, error) {
	var output MemoryOutput
	if err := build(source, emptyFeaturesByID{}, o, &output); err != nil {
		return nil, err
	}
	bytes, _, _ := output.Bytes()
	return bytes, nil
}
