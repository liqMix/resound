package resound

import "sync"

// DSPChannel represents an audio channel that can have various effects applied to it.
// Any Players that have a DSPChannel set will take on the effects applied to the channel as well.
type DSPChannel struct {
	Active      bool
	effects     map[any]IEffect
	effectOrder []IEffect
	closed      bool

	mu             sync.RWMutex
	playingPlayers []*Player
}

// NewDSPChannel returns a new DSPChannel.
func NewDSPChannel() *DSPChannel {
	dsp := &DSPChannel{
		Active:      true,
		effects:     map[any]IEffect{},
		effectOrder: []IEffect{},
	}
	return dsp
}

// Close closes the DSP channel. When closed, any players that play on the channel do not play and automatically close their sources.
// Closing the channel can be used to stop any sounds that might be playing back on the DSPChannel.
func (d *DSPChannel) Close() {
	d.closed = true
}

// AddEffect adds the specified Effect to the DSPChannel under the given identification. Note that effects added to DSPChannels don't need
// to specify source streams, as the DSPChannel automatically handles this.
func (d *DSPChannel) AddEffect(id any, effect IEffect) *DSPChannel {
	d.effects[id] = effect
	d.effectOrder = append(d.effectOrder, effect)
	return d
}

func (d *DSPChannel) Effect(id any) IEffect {
	return d.effects[id]
}

func (d *DSPChannel) addPlayerToList(p *Player) {
	d.mu.Lock()
	d.playingPlayers = append(d.playingPlayers, p)
	d.mu.Unlock()
}

func (d *DSPChannel) clean() {
	// Snapshot the slice under read lock to find a non-playing player.
	d.mu.RLock()
	snapshot := make([]*Player, len(d.playingPlayers))
	copy(snapshot, d.playingPlayers)
	d.mu.RUnlock()

	var target *Player
	for _, p := range snapshot {
		if !p.IsPlaying() {
			target = p
			break
		}
	}

	if target == nil {
		return
	}

	// Take write lock and remove by pointer identity in the current slice.
	d.mu.Lock()
	for i, p := range d.playingPlayers {
		if p == target {
			d.playingPlayers[i] = nil
			d.playingPlayers = append(d.playingPlayers[:i], d.playingPlayers[i+1:]...)
			break
		}
	}
	d.mu.Unlock()
}

// PlayingPlayers returns a copy of the list of all Players currently playing through the DSPChannel.
func (d *DSPChannel) PlayingPlayers() []*Player {
	d.mu.RLock()
	out := make([]*Player, len(d.playingPlayers))
	copy(out, d.playingPlayers)
	d.mu.RUnlock()
	return out
}

// PlayerByID returns a specific Player by its ID.
func (d *DSPChannel) PlayerByID(id any) *Player {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, p := range d.playingPlayers {
		if p.id == id {
			return p
		}
	}
	return nil
}

// IsPlayingPlayer returns if a Player with the specified ID is currently playing back.
func (d *DSPChannel) IsPlayingPlayer(id any) bool {
	d.clean()

	d.mu.RLock()
	snapshot := make([]*Player, len(d.playingPlayers))
	copy(snapshot, d.playingPlayers)
	d.mu.RUnlock()

	for _, player := range snapshot {
		if player.IsPlaying() && player.id == id {
			return true
		}
	}
	return false
}
