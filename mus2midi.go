package gore

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// MUS event types
const (
	musRelease    = 0
	musPlayNote   = 1
	musPitchBend  = 2
	musSystem     = 3
	musController = 4
	musScoreEnd   = 6
)

// MUS controller number → MIDI CC mapping.
// Controller 0 is special: it maps to a MIDI Program Change, not a CC.
var musCtrlToMIDI = [15]byte{
	0,   // 0: program change (handled specially)
	0,   // 1: bank select
	1,   // 2: modulation
	7,   // 3: volume
	10,  // 4: pan
	11,  // 5: expression
	91,  // 6: reverb
	93,  // 7: chorus
	64,  // 8: sustain pedal
	67,  // 9: soft pedal
	120, // 10: all sounds off
	123, // 11: all notes off
	126, // 12: mono
	127, // 13: poly
	121, // 14: reset all controllers
}

// MusToMidi converts DOOM MUS format data to a Standard MIDI File (format 0).
func MusToMidi(musData []byte) ([]byte, error) {
	if len(musData) < 16 || string(musData[:4]) != "MUS\x1a" {
		return nil, errors.New("mus2midi: invalid MUS header")
	}

	scoreLen := int(binary.LittleEndian.Uint16(musData[4:6]))
	scoreStart := int(binary.LittleEndian.Uint16(musData[6:8]))

	if scoreStart >= len(musData) {
		return nil, errors.New("mus2midi: score start beyond data")
	}
	if scoreStart+scoreLen > len(musData)+1 {
		return nil, errors.New("mus2midi: score length beyond data")
	}

	// Build channel map: MUS ch 15 → MIDI ch 9 (drums), others sequential skipping 9.
	var chMap [16]byte
	next := byte(0)
	for i := 0; i < 16; i++ {
		if i == 15 {
			chMap[i] = 9
		} else {
			if next == 9 {
				next++
			}
			chMap[i] = next
			next++
		}
	}

	// Per-channel volume tracking for MUS play-note events without volume byte.
	var chVol [16]byte
	for i := range chVol {
		chVol[i] = 127
	}

	var track bytes.Buffer

	// Tempo meta-event at delta 0: 500 000 µs/quarter (120 BPM).
	// With division = 70 ticks/quarter this gives 140 ticks/sec matching MUS.
	track.Write([]byte{0x00, 0xFF, 0x51, 0x03, 0x07, 0xA1, 0x20})

	pos := scoreStart
	var pendingDelay uint32

	// emitDelay writes the accumulated delay and resets it.
	// Call this immediately before writing MIDI event bytes.
	emitDelay := func() {
		writeVLQ(&track, pendingDelay)
		pendingDelay = 0
	}

	for pos < len(musData) {
		eb := musData[pos]
		pos++

		last := (eb & 0x80) != 0
		evType := (eb >> 4) & 0x07
		musCh := eb & 0x0F
		midiCh := chMap[musCh]

		switch evType {
		case musRelease:
			if pos >= len(musData) {
				break
			}
			note := musData[pos] & 0x7F
			pos++
			emitDelay()
			track.WriteByte(0x80 | midiCh)
			track.WriteByte(note)
			track.WriteByte(0)

		case musPlayNote:
			if pos >= len(musData) {
				break
			}
			nb := musData[pos]
			pos++
			note := nb & 0x7F
			vol := chVol[musCh]
			if nb&0x80 != 0 {
				if pos >= len(musData) {
					break
				}
				vol = musData[pos] & 0x7F
				pos++
				chVol[musCh] = vol
			}
			emitDelay()
			track.WriteByte(0x90 | midiCh)
			track.WriteByte(note)
			track.WriteByte(vol)

		case musPitchBend:
			if pos >= len(musData) {
				break
			}
			bend := uint16(musData[pos]) << 6
			pos++
			emitDelay()
			track.WriteByte(0xE0 | midiCh)
			track.WriteByte(byte(bend & 0x7F))
			track.WriteByte(byte((bend >> 7) & 0x7F))

		case musSystem:
			if pos >= len(musData) {
				break
			}
			ctrl := musData[pos]
			pos++
			if ctrl >= 10 && ctrl <= 14 {
				emitDelay()
				track.WriteByte(0xB0 | midiCh)
				track.WriteByte(musCtrlToMIDI[ctrl])
				track.WriteByte(0)
			}
			// Unknown system events are silently skipped; delay carries forward.

		case musController:
			if pos+1 >= len(musData) {
				break
			}
			ctrl := musData[pos]
			val := musData[pos+1]
			pos += 2
			if ctrl == 0 {
				emitDelay()
				track.WriteByte(0xC0 | midiCh)
				track.WriteByte(val & 0x7F)
			} else if ctrl < 10 {
				emitDelay()
				track.WriteByte(0xB0 | midiCh)
				track.WriteByte(musCtrlToMIDI[ctrl])
				track.WriteByte(val & 0x7F)
			}
			// Unknown controllers are silently skipped; delay carries forward.

		case musScoreEnd:
			goto done
		}

		if last {
			var delay uint32
			for pos < len(musData) {
				b := musData[pos]
				pos++
				delay = delay*128 + uint32(b&0x7F)
				if b&0x80 == 0 {
					break
				}
			}
			pendingDelay += delay
		}
	}

done:
	// End-of-track meta-event with any remaining delay.
	writeVLQ(&track, pendingDelay)
	track.Write([]byte{0xFF, 0x2F, 0x00})

	// Assemble Standard MIDI File (format 0, 1 track, division = 70).
	var midi bytes.Buffer
	midi.WriteString("MThd")
	binary.Write(&midi, binary.BigEndian, uint32(6))
	binary.Write(&midi, binary.BigEndian, uint16(0)) // format 0
	binary.Write(&midi, binary.BigEndian, uint16(1)) // 1 track
	binary.Write(&midi, binary.BigEndian, uint16(70))
	midi.WriteString("MTrk")
	binary.Write(&midi, binary.BigEndian, uint32(track.Len()))
	midi.Write(track.Bytes())

	return midi.Bytes(), nil
}

// writeVLQ writes a MIDI variable-length quantity.
func writeVLQ(buf *bytes.Buffer, value uint32) {
	if value == 0 {
		buf.WriteByte(0)
		return
	}
	var vlq [4]byte
	n := 0
	for value > 0 {
		vlq[n] = byte(value & 0x7F)
		value >>= 7
		n++
	}
	for i := n - 1; i >= 0; i-- {
		b := vlq[i]
		if i > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
}
