package main

import (
	"bytes"
	"fmt"
	"image/color"
	"time"

	_ "embed"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/audio"
	"github.com/hajimehoshi/ebiten/v2/audio/vorbis"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/text"
	"github.com/solarlune/resound"
	"github.com/solarlune/resound/effects"
	"golang.org/x/image/font/basicfont"
)

type Game struct {
	TimeStretch *effects.TimeStretch
}

//go:embed song.ogg
var songData []byte

const sampleRate = 44100

func NewGame() *Game {

	audio.NewContext(sampleRate)

	reader := bytes.NewReader(songData)

	stream, err := vorbis.DecodeWithSampleRate(sampleRate, reader)
	if err != nil {
		panic(err)
	}

	loop := audio.NewInfiniteLoop(stream, stream.Length())

	// Create a TimeStretch effect wrapping the loop as a source.
	// This changes playback speed while preserving pitch.
	game := &Game{
		TimeStretch: effects.NewTimeStretch().SetSource(loop).SetSpeed(1.0),
	}

	// Pass the TimeStretch as the source to the Player.
	player, err := resound.NewPlayer("bgm", game.TimeStretch)
	if err != nil {
		panic(err)
	}

	player.SetBufferSize(time.Millisecond * 50)

	player.Play()

	return game
}

func (game *Game) Update() error {

	var err error

	if inpututil.IsKeyJustPressed(ebiten.KeySpace) {
		game.TimeStretch.SetActive(!game.TimeStretch.Active())
	}

	if inpututil.IsKeyJustPressed(ebiten.KeyUp) {
		game.TimeStretch.SetSpeed(game.TimeStretch.Speed() + 0.05)
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyDown) {
		game.TimeStretch.SetSpeed(game.TimeStretch.Speed() - 0.05)
	}

	if inpututil.IsKeyJustPressed(ebiten.Key1) {
		game.TimeStretch.SetQuality(effects.QualityLow)
	}
	if inpututil.IsKeyJustPressed(ebiten.Key2) {
		game.TimeStretch.SetQuality(effects.QualityMedium)
	}
	if inpututil.IsKeyJustPressed(ebiten.Key3) {
		game.TimeStretch.SetQuality(effects.QualityHigh)
	}

	if ebiten.IsKeyPressed(ebiten.KeyEscape) {
		err = ebiten.Termination
	}

	return err
}

func (game *Game) Draw(screen *ebiten.Image) {

	qualityName := "Medium"
	switch game.TimeStretch.Quality() {
	case effects.QualityLow:
		qualityName = "Low"
	case effects.QualityHigh:
		qualityName = "High"
	}

	text.Draw(screen, fmt.Sprintf(`This example shows how the TimeStretch
effect works. It changes playback speed
while preserving pitch.

Up/Down: Adjust speed (0.25x - 4.0x)
Space:   Toggle effect on/off
1/2/3:   Set quality (Low/Medium/High)

Active:  %t
Speed:   %.2fx
Quality: %s`, game.TimeStretch.Active(), game.TimeStretch.Speed(), qualityName), basicfont.Face7x13, 16, 16, color.White)

}

func (game *Game) Layout(w, h int) (int, int) { return 320, 240 }

func main() {

	ebiten.SetWindowTitle("Resound Demo - Time Stretch")
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)

	if err := ebiten.RunGame(NewGame()); err != nil {
		panic(err)
	}

}
