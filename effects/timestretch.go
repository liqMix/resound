package effects

import (
	"io"
	"math"
	"sync"
	"sync/atomic"

	"github.com/solarlune/resound"
)

// Quality represents the quality level for the TimeStretch effect. Higher
// quality uses longer crossfades and wider alignment searches, costing a
// little more CPU per processed cycle.
type Quality int

const (
	QualityLow    Quality = iota // overlap 6ms, seek ±10ms, fixed 45ms sequence
	QualityMedium                // overlap 8ms, seek ±12ms, auto 50-100ms sequence
	QualityHigh                  // overlap 12ms, seek ±14ms, auto 60-125ms sequence
)

// Playback modes. Transitions are resolved lazily inside Read so that all
// engine state is only ever touched on the audio goroutine.
const (
	modeBypass  = iota // speed == 1.0: raw, bit-exact passthrough
	modeStretch        // speed != 1.0: time-stretch engine active
	modeFlush          // returning to 1.0: drain engine output, then seek source
)

// emitChunk records how many source frames each emitted output frame
// represents, so SourcePosition can be reported exactly even across
// mid-stream speed changes (output queued at an old speed is still
// accounted at that speed).
type emitChunk struct {
	frames      int
	srcPerFrame float64 // source frames represented by one output frame
}

// TimeStretch changes the playback speed of audio while preserving its pitch,
// using a SoundTouch-style WSOLA (Waveform Similarity Overlap-Add) algorithm:
// fixed-length output sequences are taken from the input at a position chosen
// by cross-correlation around the nominal (exact-tempo) read position, joined
// by a short linear crossfade.
//
// Unlike most effects in resound, TimeStretch cannot be used as an in-place
// effect via AddEffect() because time-stretching decouples source consumption
// from output production. Instead, use it as a source wrapper:
//
//	ts := effects.NewTimeStretch().SetSource(loop).SetSpeed(0.75)
//	player, _ := resound.NewPlayer("bgm", ts)
//	player.Play()
//
// ApplyEffect() is a no-op on this effect.
//
// Units: one "frame" is one stereo sample frame = 4 source bytes. Interleaved
// stereo float64 slices have length frames*2; mono slices have length frames.
// Bytes appear only at the Read/Seek boundary.
type TimeStretch struct {
	Source io.ReadSeeker

	mu          sync.Mutex
	targetSpeed atomic.Uint64 // math.Float64bits; written by SetSpeed, read at cycle start
	speed       float64       // speed in effect for the current cycle
	active      bool
	quality     Quality
	sampleRate  int

	// Derived parameters (frames), fixed per quality+sampleRate.
	overlap    int // crossfade length
	seekRadius int // one-sided alignment search radius around the nominal position
	seqMinMS   int // auto-sequence clamp, milliseconds
	seqMaxMS   int
	fadeTotal  int // post-reset declick fade-in length

	mode int

	// Input FIFO. Frame 0 of inStereo/inMono is source frame inBaseAbs.
	// Invariant: len(inStereo) == inFrames*2, len(inMono) == inFrames.
	inStereo   []float64
	inMono     []float64 // (L+R)/2, used for correlation
	inFrames   int
	inBaseAbs  int64   // absolute source frame index of input frame 0
	nominalPos float64 // fractional input-frame index of the next ideal sequence start

	// Correlation/crossfade reference: the raw (unwindowed) tail of the
	// previously emitted sequence, i.e. the exact natural continuation of
	// what the listener last heard.
	refStereo []float64 // overlap*2
	refMono   []float64 // overlap
	refEnergy float64
	refValid  bool
	refEndAbs int64 // absolute source frame just past the reference tail

	// Output FIFO (interleaved stereo float64). outRead indexes elements.
	outBuf  []float64
	outRead int

	flushTargetAbs int64 // source frame to seek to when a flush completes

	// Position reporting. Byte-valued at the API boundary.
	srcReadBytes int64   // bytes consumed from Source
	srcEmitted   float64 // source bytes corresponding to the next output sample
	outEmitted   int64   // output bytes handed to the caller since last Seek/SetSource
	emitQueue    []emitChunk
	fadeInLeft   int // declick fade frames remaining after a reset

	sourceErr error  // io.EOF or terminal source error
	readBuf   []byte // source read scratch
	carryLen  int    // partial-frame bytes carried at the front of readBuf
}

// NewTimeStretch creates a new TimeStretch effect with QualityMedium,
// speed 1.0, and a 44100Hz sample rate.
func NewTimeStretch() *TimeStretch {
	ts := &TimeStretch{
		speed:      1.0,
		active:     true,
		quality:    QualityMedium,
		sampleRate: 44100,
	}
	ts.targetSpeed.Store(math.Float64bits(1.0))
	ts.initParams()
	return ts
}

// Clone clones the effect, returning a resound.IEffect.
func (ts *TimeStretch) Clone() resound.IEffect {
	c := &TimeStretch{
		Source:     ts.Source,
		speed:      ts.speed,
		active:     ts.active,
		quality:    ts.quality,
		sampleRate: ts.sampleRate,
	}
	c.targetSpeed.Store(ts.targetSpeed.Load())
	c.initParams()
	return c
}

func (ts *TimeStretch) msToFrames(ms int) int {
	return ms * ts.sampleRate / 1000
}

// initParams derives frame-based parameters from quality and sample rate,
// allocates all processing buffers, and resets engine state.
func (ts *TimeStretch) initParams() {
	switch ts.quality {
	case QualityLow:
		ts.overlap = ts.msToFrames(6)
		ts.seekRadius = ts.msToFrames(10)
		ts.seqMinMS, ts.seqMaxMS = 45, 45
	case QualityHigh:
		ts.overlap = ts.msToFrames(12)
		ts.seekRadius = ts.msToFrames(14)
		ts.seqMinMS, ts.seqMaxMS = 60, 125
	default: // QualityMedium
		ts.overlap = ts.msToFrames(8)
		ts.seekRadius = ts.msToFrames(12)
		ts.seqMinMS, ts.seqMaxMS = 50, 100
	}
	ts.fadeTotal = ts.msToFrames(2)

	seqMax := ts.msToFrames(ts.seqMaxMS)
	// Worst case input residency: search history + one sequence + the
	// largest per-cycle advance (speed 4.0).
	inCap := 2*ts.seekRadius + 2*seqMax + 4*seqMax
	ts.inStereo = make([]float64, 0, inCap*2)
	ts.inMono = make([]float64, 0, inCap)

	// Output: a couple of sequences plus a generous caller request.
	outCap := 2*seqMax + 16384
	ts.outBuf = make([]float64, 0, outCap*2)

	ts.refStereo = make([]float64, ts.overlap*2)
	ts.refMono = make([]float64, ts.overlap)
	ts.emitQueue = make([]emitChunk, 0, 64)
	ts.readBuf = make([]byte, 16384)

	ts.resetEngine()
	ts.mode = modeBypass
	ts.fadeInLeft = ts.fadeTotal
}

// resetEngine clears all streaming state but leaves position counters to the
// caller (Seek and SetSource set them from the source's actual position).
func (ts *TimeStretch) resetEngine() {
	ts.inStereo = ts.inStereo[:0]
	ts.inMono = ts.inMono[:0]
	ts.inFrames = 0
	ts.nominalPos = 0
	ts.refValid = false
	ts.refEnergy = 0
	ts.outBuf = ts.outBuf[:0]
	ts.outRead = 0
	ts.emitQueue = ts.emitQueue[:0]
	ts.flushTargetAbs = 0
	ts.carryLen = 0
	ts.sourceErr = nil
}

func (ts *TimeStretch) loadTargetSpeed() float64 {
	return math.Float64frombits(ts.targetSpeed.Load())
}

func (ts *TimeStretch) pendingOutFrames() int {
	return (len(ts.outBuf) - ts.outRead) / 2
}

// autoSeqFrames returns the sequence length for the given speed: longer
// sequences at slow tempos (fewer, gentler joins), shorter at fast tempos
// (joins closer together so content isn't skipped in big steps).
func (ts *TimeStretch) autoSeqFrames(speed float64) int {
	ms := clamp(150.0-50.0*speed, float64(ts.seqMinMS), float64(ts.seqMaxMS))
	return int(ms * float64(ts.sampleRate) / 1000.0)
}

// Read fills p with audio data, time-stretched when speed != 1.0.
func (ts *TimeStretch) Read(p []byte) (int, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.Source == nil {
		return 0, io.EOF
	}
	if !ts.active {
		return ts.readBypass(p)
	}

	ts.resolveTransitions()

	switch ts.mode {
	case modeBypass:
		return ts.readBypass(p)

	case modeFlush:
		framesWanted := len(p) / 4
		n := ts.emit(p, framesWanted)
		if ts.pendingOutFrames() == 0 {
			ts.finishFlush()
			if n < framesWanted {
				m, err := ts.readBypass(p[n*4:])
				total := n*4 + m
				if total == 0 {
					return 0, err
				}
				if err != nil && err != io.EOF {
					return total, err
				}
				return total, nil
			}
		}
		return n * 4, nil
	}

	// modeStretch
	framesWanted := len(p) / 4
	if framesWanted == 0 {
		return 0, nil
	}

	for ts.pendingOutFrames() < framesWanted && ts.sourceErr == nil {
		if !ts.runCycle() {
			break
		}
	}

	n := ts.emit(p, framesWanted)
	if n == 0 {
		if ts.sourceErr != nil {
			return 0, ts.sourceErr
		}
		return 0, nil
	}
	return n * 4, nil
}

// readBypass forwards a raw read from the source, keeping position counters
// in sync. Used at speed 1.0 and when inactive; output is bit-exact.
func (ts *TimeStretch) readBypass(p []byte) (int, error) {
	n, err := ts.Source.Read(p)
	if n > 0 {
		ts.srcReadBytes += int64(n)
		ts.srcEmitted += float64(n)
		ts.outEmitted += int64(n)
	}
	return n, err
}

// resolveTransitions applies a pending speed-target change at a safe point.
// A flush in progress always completes first; the next Read re-engages the
// stretcher if the target moved away from 1.0 again in the meantime.
func (ts *TimeStretch) resolveTransitions() {
	target := ts.loadTargetSpeed()
	switch ts.mode {
	case modeBypass:
		if target != 1.0 {
			ts.startStretch()
		}
	case modeStretch:
		if target == 1.0 {
			ts.startFlush()
		}
	}
}

// startStretch engages the engine mid-stream. The engine starts empty at the
// source's current position, and the first cycle emits its sequence verbatim
// from that exact frame, so the transition is waveform-continuous.
func (ts *TimeStretch) startStretch() {
	ts.resetEngine()
	ts.inBaseAbs = ts.srcReadBytes / 4
	ts.mode = modeStretch
}

// startFlush begins the return to passthrough: queue the reference tail (the
// exact next content of the output stream), then once the queue drains,
// finishFlush seeks the source to the frame following it.
func (ts *TimeStretch) startFlush() {
	if ts.refValid {
		ts.outBuf = append(ts.outBuf, ts.refStereo...)
		ts.pushEmit(ts.overlap, 1.0)
		ts.flushTargetAbs = ts.refEndAbs
	} else {
		ts.flushTargetAbs = ts.inBaseAbs + int64(math.Round(ts.nominalPos))
	}
	// Unconsumed input is discarded; the flush seek re-positions the source.
	ts.inStereo = ts.inStereo[:0]
	ts.inMono = ts.inMono[:0]
	ts.inFrames = 0
	ts.refValid = false
	ts.carryLen = 0
	ts.mode = modeFlush
}

func (ts *TimeStretch) finishFlush() {
	pos, err := ts.Source.Seek(ts.flushTargetAbs*4, io.SeekStart)
	ts.srcReadBytes = pos
	ts.srcEmitted = float64(pos)
	ts.sourceErr = nil
	ts.mode = modeBypass
	if err != nil {
		ts.sourceErr = err
	}
}

// emit drains up to framesWanted frames of pending output into p, advancing
// the position accounting. Returns frames written.
func (ts *TimeStretch) emit(p []byte, framesWanted int) int {
	n := ts.pendingOutFrames()
	if n > framesWanted {
		n = framesWanted
	}
	if n == 0 {
		return 0
	}

	ab := resound.AudioBuffer(p)
	for i := 0; i < n; i++ {
		l := ts.outBuf[ts.outRead+i*2]
		r := ts.outBuf[ts.outRead+i*2+1]
		if ts.fadeInLeft > 0 {
			g := 1.0 - float64(ts.fadeInLeft)/float64(ts.fadeTotal)
			l *= g
			r *= g
			ts.fadeInLeft--
		}
		ab.Set(i, l, r)
	}
	ts.outRead += n * 2
	if ts.outRead == len(ts.outBuf) {
		ts.outBuf = ts.outBuf[:0]
		ts.outRead = 0
	}

	rem := n
	for rem > 0 && len(ts.emitQueue) > 0 {
		c := &ts.emitQueue[0]
		take := c.frames
		if take > rem {
			take = rem
		}
		ts.srcEmitted += float64(take) * c.srcPerFrame * 4.0
		c.frames -= take
		rem -= take
		if c.frames == 0 {
			copy(ts.emitQueue, ts.emitQueue[1:])
			ts.emitQueue = ts.emitQueue[:len(ts.emitQueue)-1]
		}
	}
	ts.outEmitted += int64(n) * 4

	return n
}

func (ts *TimeStretch) pushEmit(frames int, srcPerFrame float64) {
	if frames <= 0 {
		return
	}
	if k := len(ts.emitQueue); k > 0 && ts.emitQueue[k-1].srcPerFrame == srcPerFrame {
		ts.emitQueue[k-1].frames += frames
		return
	}
	ts.emitQueue = append(ts.emitQueue, emitChunk{frames: frames, srcPerFrame: srcPerFrame})
}

// compactOut moves unread output to the front of outBuf so cycles can append.
func (ts *TimeStretch) compactOut() {
	if ts.outRead == 0 {
		return
	}
	copy(ts.outBuf, ts.outBuf[ts.outRead:])
	ts.outBuf = ts.outBuf[:len(ts.outBuf)-ts.outRead]
	ts.outRead = 0
}

// runCycle produces one sequence of output (seq − overlap frames) and
// consumes exactly speed × (seq − overlap) input frames via the fractional
// nominalPos accumulator, so the long-term tempo is exact at any speed.
// Returns false if no progress could be made (caller breaks).
func (ts *TimeStretch) runCycle() bool {
	ts.compactOut()

	speed := ts.loadTargetSpeed()
	if speed == 1.0 {
		// Speed snapped back to 1.0 between emits; switch to flushing.
		ts.resolveTransitions()
		return false
	}
	ts.speed = speed
	seq := ts.autoSeqFrames(speed)
	ov := ts.overlap

	need := int(ts.nominalPos) + ts.seekRadius + seq + 1
	ts.fillInput(need)

	nom := int(math.Round(ts.nominalPos))
	var off int
	if !ts.refValid {
		off = nom
		if off+seq > ts.inFrames {
			if ts.sourceErr != nil {
				ts.flushEOF()
				return true
			}
			return false
		}
	} else {
		lo := nom - ts.seekRadius
		if lo < 0 {
			lo = 0
		}
		hi := ts.inFrames - seq
		if hi > nom+ts.seekRadius {
			hi = nom + ts.seekRadius
		}
		if hi < lo {
			if ts.sourceErr != nil {
				ts.flushEOF()
				return true
			}
			return false
		}
		off = ts.findBestOffset(lo, hi, nom)
	}

	if ts.refValid {
		// Crossfade from the reference tail (what the listener last heard)
		// into the newly aligned content, then copy the rest verbatim.
		for i := 0; i < ov; i++ {
			w := float64(i) / float64(ov)
			l := ts.refStereo[i*2]*(1.0-w) + ts.inStereo[(off+i)*2]*w
			r := ts.refStereo[i*2+1]*(1.0-w) + ts.inStereo[(off+i)*2+1]*w
			ts.outBuf = append(ts.outBuf, l, r)
		}
		ts.outBuf = append(ts.outBuf, ts.inStereo[(off+ov)*2:(off+seq-ov)*2]...)
	} else {
		// First cycle after a reset or mode switch: emit verbatim from the
		// exact next source frame for waveform continuity.
		ts.outBuf = append(ts.outBuf, ts.inStereo[off*2:(off+seq-ov)*2]...)
	}

	// The new reference tail is the unemitted continuation of this sequence.
	copy(ts.refStereo, ts.inStereo[(off+seq-ov)*2:(off+seq)*2])
	copy(ts.refMono, ts.inMono[off+seq-ov:off+seq])
	ts.refEnergy = 0
	for _, v := range ts.refMono {
		ts.refEnergy += v * v
	}
	ts.refValid = true
	ts.refEndAbs = ts.inBaseAbs + int64(off+seq)

	out := seq - ov
	ts.nominalPos += speed * float64(out)
	ts.pushEmit(out, speed)

	ts.dropInput()
	return true
}

// findBestOffset searches [lo, hi] for the start position whose first overlap
// frames best continue the reference tail, scored by normalized
// cross-correlation with a small bias toward the nominal position. Two-stage:
// coarse step 8, then refine ±7 around the coarse best.
func (ts *TimeStretch) findBestOffset(lo, hi, nom int) int {
	if ts.refEnergy < 1e-9 {
		// Silent reference: any join is inaudible, stay on the nominal timeline.
		if nom < lo {
			return lo
		}
		if nom > hi {
			return hi
		}
		return nom
	}

	best := lo
	bestScore := math.Inf(-1)
	for pos := lo; pos <= hi; pos += 8 {
		if s := ts.scoreAt(pos, nom); s > bestScore {
			bestScore = s
			best = pos
		}
	}
	rlo, rhi := best-7, best+7
	if rlo < lo {
		rlo = lo
	}
	if rhi > hi {
		rhi = hi
	}
	for pos := rlo; pos <= rhi; pos++ {
		if pos == best {
			continue
		}
		if s := ts.scoreAt(pos, nom); s > bestScore {
			bestScore = s
			best = pos
		}
	}
	return best
}

func (ts *TimeStretch) scoreAt(pos, nom int) float64 {
	var dot, candEnergy float64
	cand := ts.inMono[pos : pos+ts.overlap]
	for i, rv := range ts.refMono {
		cv := cand[i]
		dot += rv * cv
		candEnergy += cv * cv
	}
	denom := math.Sqrt(ts.refEnergy * candEnergy)
	ncc := 0.0
	if denom > 1e-10 {
		ncc = dot / denom
	}
	dev := math.Abs(float64(pos - nom))
	return ncc - 0.02*dev/float64(ts.seekRadius)
}

// fillInput reads from the source until inFrames >= needFrames or the source
// errors, converting PCM bytes to float64 and carrying partial frames.
func (ts *TimeStretch) fillInput(needFrames int) {
	for ts.inFrames < needFrames && ts.sourceErr == nil {
		want := (needFrames-ts.inFrames)*4 - ts.carryLen
		if want > len(ts.readBuf)-ts.carryLen {
			want = len(ts.readBuf) - ts.carryLen
		}
		if want <= 0 {
			want = 4
		}
		n, err := ts.Source.Read(ts.readBuf[ts.carryLen : ts.carryLen+want])
		if n > 0 {
			ts.srcReadBytes += int64(n)
		}
		total := ts.carryLen + n
		frames := total / 4
		if frames > 0 {
			ab := resound.AudioBuffer(ts.readBuf[:frames*4])
			for i := 0; i < frames; i++ {
				l, r := ab.Get(i)
				ts.inStereo = append(ts.inStereo, l, r)
				ts.inMono = append(ts.inMono, (l+r)*0.5)
			}
			ts.inFrames += frames
		}
		ts.carryLen = total - frames*4
		if ts.carryLen > 0 {
			copy(ts.readBuf, ts.readBuf[frames*4:total])
		}
		if err != nil {
			ts.sourceErr = err
			return
		}
		if n == 0 {
			return // avoid spinning on a (0, nil) source
		}
	}
}

// dropInput discards input history more than seekRadius before the nominal
// position, keeping enough behind the playhead for the alignment search.
func (ts *TimeStretch) dropInput() {
	drop := int(ts.nominalPos) - ts.seekRadius
	if drop <= 0 {
		return
	}
	if drop > ts.inFrames {
		drop = ts.inFrames
	}
	copy(ts.inStereo, ts.inStereo[drop*2:])
	copy(ts.inMono, ts.inMono[drop:])
	ts.inFrames -= drop
	ts.inStereo = ts.inStereo[:ts.inFrames*2]
	ts.inMono = ts.inMono[:ts.inFrames]
	ts.nominalPos -= float64(drop)
	ts.inBaseAbs += int64(drop)
}

// flushEOF drains what remains of the input when the source ends: one last
// crossfaded join onto the remaining content where possible, so the stream
// ends without an amplitude step.
func (ts *TimeStretch) flushEOF() {
	nom := int(math.Round(ts.nominalPos))
	ov := ts.overlap

	switch {
	case ts.refValid && ts.inFrames >= ov:
		off := nom
		if off > ts.inFrames-ov {
			off = ts.inFrames - ov
		}
		if off < 0 {
			off = 0
		}
		for i := 0; i < ov; i++ {
			w := float64(i) / float64(ov)
			l := ts.refStereo[i*2]*(1.0-w) + ts.inStereo[(off+i)*2]*w
			r := ts.refStereo[i*2+1]*(1.0-w) + ts.inStereo[(off+i)*2+1]*w
			ts.outBuf = append(ts.outBuf, l, r)
		}
		ts.outBuf = append(ts.outBuf, ts.inStereo[(off+ov)*2:]...)
		ts.pushEmit(ts.inFrames-off, ts.speed)

	case ts.refValid:
		// Not even one overlap of input left: fade the reference tail out.
		for i := 0; i < ov; i++ {
			g := 1.0 - float64(i)/float64(ov)
			ts.outBuf = append(ts.outBuf, ts.refStereo[i*2]*g, ts.refStereo[i*2+1]*g)
		}
		ts.pushEmit(ov, ts.speed)

	default:
		ts.outBuf = append(ts.outBuf, ts.inStereo...)
		ts.pushEmit(ts.inFrames, 1.0)
	}

	ts.inStereo = ts.inStereo[:0]
	ts.inMono = ts.inMono[:0]
	ts.inFrames = 0
	ts.refValid = false
	ts.nominalPos = 0
}

// Seek resets internal state and seeks the underlying source. Offsets are
// source-stream bytes (the same coordinate space the source itself uses), so
// player position-setting flows through unchanged.
func (ts *TimeStretch) Seek(offset int64, whence int) (int64, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.Source == nil {
		return 0, nil
	}

	// A pure position query must not disturb streaming state.
	if offset == 0 && whence == io.SeekCurrent {
		return ts.Source.Seek(0, io.SeekCurrent)
	}

	ts.resetEngine()
	ts.mode = modeBypass
	ts.fadeInLeft = ts.fadeTotal
	pos, err := ts.Source.Seek(offset, whence)
	ts.srcReadBytes = pos
	ts.srcEmitted = float64(pos)
	ts.inBaseAbs = pos / 4
	ts.outEmitted = 0
	return pos, err
}

// ApplyEffect is a no-op for TimeStretch. This effect cannot be used as an
// in-place effect via AddEffect() because time-stretching changes the
// relationship between input and output sizes. Use it as a source wrapper instead.
func (ts *TimeStretch) ApplyEffect(data []byte, bytesRead int) {
	// No-op: TimeStretch must be used as a source, not an in-place effect.
}

// SetSource sets the source stream for the TimeStretch effect and resets all state.
func (ts *TimeStretch) SetSource(source io.ReadSeeker) *TimeStretch {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.Source = source
	ts.resetEngine()
	ts.mode = modeBypass
	ts.fadeInLeft = ts.fadeTotal
	ts.srcReadBytes = 0
	ts.srcEmitted = 0
	ts.inBaseAbs = 0
	ts.outEmitted = 0
	return ts
}

// SetSpeed sets the playback speed multiplier. Values less than 1.0 slow
// down playback (longer duration), values greater than 1.0 speed it up.
// The value is clamped to the range [0.25, 4.0]. The change is picked up at
// the next processing cycle on the audio goroutine; transitions (including
// to and from 1.0) are click-free.
func (ts *TimeStretch) SetSpeed(speed float64) *TimeStretch {
	ts.targetSpeed.Store(math.Float64bits(clamp(speed, 0.25, 4.0)))
	return ts
}

// Speed returns the current playback speed multiplier.
func (ts *TimeStretch) Speed() float64 {
	return ts.loadTargetSpeed()
}

// SourcePosition returns the absolute position in the source stream, in
// bytes (the same units Seek uses), corresponding to the next output sample
// this effect will hand to its caller. Internal buffering latency is
// accounted for; latency downstream of this effect (e.g. a player's buffer)
// is not, and can be compensated using OutputPosition.
func (ts *TimeStretch) SourcePosition() int64 {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return int64(ts.srcEmitted)
}

// OutputPosition returns the number of output bytes emitted since the last
// Seek or SetSource. Comparing this against how much the downstream player
// has actually played reveals how much output sits in downstream buffers.
func (ts *TimeStretch) OutputPosition() int64 {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.outEmitted
}

// SetQuality sets the quality level. This reinitializes internal buffers and
// resets streaming state, so it is intended to be called at construction
// time, not mid-playback.
func (ts *TimeStretch) SetQuality(quality Quality) *TimeStretch {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.quality = quality
	ts.initParams()
	return ts
}

// Quality returns the current quality setting.
func (ts *TimeStretch) Quality() Quality {
	return ts.quality
}

// SetSampleRate sets the sample rate used to derive frame-based processing
// parameters. Defaults to 44100. This reinitializes internal buffers and
// resets streaming state, so call it at construction time.
func (ts *TimeStretch) SetSampleRate(rate int) *TimeStretch {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if rate > 0 {
		ts.sampleRate = rate
		ts.initParams()
	}
	return ts
}

// SampleRate returns the configured sample rate.
func (ts *TimeStretch) SampleRate() int {
	return ts.sampleRate
}

// SetActive sets whether the effect is active. When inactive, Read() passes
// through directly from the source regardless of speed. Toggling this while
// stretched playback is in progress jumps the source position; prefer
// SetSpeed(1.0), which transitions continuously.
func (ts *TimeStretch) SetActive(active bool) *TimeStretch {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.active = active
	return ts
}

// Active returns whether the effect is active.
func (ts *TimeStretch) Active() bool {
	return ts.active
}
