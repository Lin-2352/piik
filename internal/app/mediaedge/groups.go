package mediaedge

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TNTcraftHIM/Piik/internal/diagnostics"
	"github.com/TNTcraftHIM/Piik/internal/media/encoded"
	"github.com/TNTcraftHIM/Piik/internal/media/forwarding"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Transport owns current attachment; this map owns only requested membership.
// An independent keyframe installs a requested source without replacing its PC.
type groupConsumer interface {
	CurrentSource() *forwarding.Source
	ReplaceSource(*forwarding.Source) error
}

type outputGroup struct {
	slot   int
	media  *forwarding.EncodedSource
	budget uint32
	active bool
	start  time.Duration
}

type outputDemand struct {
	consumer groupConsumer
	budget   uint32
	full     uint32 // held budget against the full-resolution ceiling
	active   bool
	lower    bool
	original bool
}

func (source *Source) groupForMedia(media *forwarding.Source) *outputGroup {
	for _, group := range source.groups {
		if group.media.Source == media {
			return group
		}
	}
	return nil
}

// Budget hysteresis: an estimate that follows the encoder's own output must not
// retarget the encoder every frame. Budgets move in 5% steps of the ceiling; a
// rise waits for sustained headroom, a fall waits briefly unless it is large.
const (
	budgetSteps      = 20
	budgetRaiseAfter = 2 * time.Second
	budgetLowerAfter = time.Second
	budgetDropRatio  = 0.7 // a larger fall is congestion, applied at once
)

type heldBudget struct {
	value   uint32
	pending time.Time // since when the estimate disagrees in one direction
	rising  bool
}

func (source *Source) clock() time.Time {
	if source.now != nil {
		return source.now()
	}
	return time.Now()
}

func (source *Source) holdBudget(consumer groupConsumer, raw, ceiling uint32) uint32 {
	step := max(ceiling/budgetSteps, 1000)
	quantized := max(step, raw/step*step)
	if raw >= ceiling {
		quantized = ceiling
	}
	if source.heldBudgets == nil {
		source.heldBudgets = make(map[groupConsumer]heldBudget)
	}
	now := source.clock()
	held, ok := source.heldBudgets[consumer]
	switch {
	case !ok, quantized < uint32(float64(held.value)*budgetDropRatio):
		held = heldBudget{value: quantized}
	case quantized == held.value:
		held.pending = time.Time{}
	default:
		rising := quantized > held.value
		if held.pending.IsZero() || held.rising != rising {
			held.pending, held.rising = now, rising
		}
		wait := budgetLowerAfter
		if rising {
			wait = budgetRaiseAfter
		}
		if now.Sub(held.pending) >= wait {
			held = heldBudget{value: quantized}
		}
	}
	source.heldBudgets[consumer] = held
	return held.value
}

func (source *Source) collectDemands(edges []*Edge, publications []*Publication) []outputDemand {
	demands := make([]outputDemand, 0, len(edges)+len(publications))
	ceiling := source.media.TrackInfo().Layers[0].Bitrate
	top := ceiling
	if len(source.outputBitrates) > 1 && source.outputBitrates[1] > 0 {
		top = source.outputBitrates[1]
	}
	present := make(map[groupConsumer]bool, len(edges)+len(publications))
	for _, edge := range edges {
		state := edge.transport.Output.State()
		_, observed := edge.transport.TargetBitrate()
		active := edge.State() == webrtc.PeerConnectionStateConnected && state.VideoBudget > 0
		lower := active && !edge.local && observed &&
			(state.Target == 0 || state.Current == 0 && !state.Paused || state.Paused && state.Prepare == 0)
		full := source.holdBudget(edge.transport, uint32(max(1000, min(state.VideoBudget, int64(top)))), top)
		demands = append(demands, outputDemand{consumer: edge.transport, active: active, lower: lower,
			original: edge.transport.RequiredActiveCount() > 1, full: full, budget: min(full, ceiling)})
		present[edge.transport] = true
	}
	for _, publication := range publications {
		required := publication.RequiredActiveCount()
		active := required > 0
		budget := publication.LowestLayerBudget()
		if budget <= 0 {
			budget = int64(ceiling)
		}
		held := source.holdBudget(publication.transport, uint32(max(1000, min(budget, int64(ceiling)))), ceiling)
		demands = append(demands, outputDemand{consumer: publication.transport, active: active, lower: active,
			original: required > 1, full: held, budget: held})
		present[publication.transport] = true
	}
	for consumer := range source.heldBudgets {
		if !present[consumer] {
			delete(source.heldBudgets, consumer)
		}
	}
	return demands
}

// Equal connection-level requests share one correction/adaptation owner. The
// group's RTP-to-codec conversion is applied once, not used as a membership key.
func (source *Source) planGroups(demands []outputDemand) (OutputPlan, error) {
	plan := OutputPlan{Active: make([]bool, len(source.outputBitrates)),
		Bitrates: append([]uint32(nil), source.outputBitrates...)}
	if len(source.formats) != 2 || len(plan.Bitrates) < 2 {
		return plan, errors.New("shared output groups need an original and derived slot")
	}
	if source.groups == nil {
		source.groups = map[int]*outputGroup{0: {slot: 0, media: source.media, budget: source.outputBitrates[0]}}
		source.memberships = make(map[groupConsumer]*outputGroup)
	}
	present := make(map[groupConsumer]bool, len(demands))
	hasLowerDemand := false
	for _, demand := range demands {
		present[demand.consumer] = true
		hasLowerDemand = hasLowerDemand || demand.active && demand.lower
		plan.Active[1] = plan.Active[1] || demand.active && demand.original
	}
	for consumer := range source.memberships {
		if !present[consumer] {
			delete(source.memberships, consumer)
		}
	}
	compatible := func(group *outputGroup, demand outputDemand) bool {
		for _, other := range demands {
			if other.active && other.lower && other.budget != demand.budget &&
				(other.consumer.CurrentSource() == group.media.Source || source.memberships[other.consumer] == group) {
				return false
			}
		}
		return true
	}
	// Revalidate pending joins before they can prevent an incumbent from
	// adjusting in place. Membership order must not give stale demand priority.
	for _, demand := range demands {
		group := source.memberships[demand.consumer]
		if demand.active && demand.lower && group != nil && demand.consumer.CurrentSource() != group.media.Source && !compatible(group, demand) {
			delete(source.memberships, demand.consumer)
		}
	}
	// The previous frame's active bit alone may outlive its last handoff.
	inUse := func(group *outputGroup) bool {
		if group == nil || !group.active {
			return false
		}
		for _, demand := range demands {
			if demand.active && (demand.consumer.CurrentSource() == group.media.Source || source.memberships[demand.consumer] == group) {
				return true
			}
		}
		return false
	}
	for _, demand := range demands {
		if !demand.active || !demand.lower {
			if source.memberships[demand.consumer] != nil {
				source.groupRecovery.Or(2)
			}
			source.memberships[demand.consumer] = nil
			continue
		}
		var selected *outputGroup
		for slot := 0; slot < len(source.outputBitrates); slot++ {
			group := source.groups[slot]
			if group != nil && source.outputBitrates[slot] > 0 && group.budget == demand.budget {
				if demand.consumer.CurrentSource() != group.media.Source && !compatible(group, demand) {
					continue
				}
				if selected == nil || inUse(group) {
					selected = group
				}
				if inUse(group) {
					break
				}
			}
		}
		if selected == nil || !inUse(selected) {
			// A group's rates can change in place only when no other current or
			// requested member still needs a different budget.
			owned := []*outputGroup{source.memberships[demand.consumer], source.groupForMedia(demand.consumer.CurrentSource())}
			var ordered []*outputGroup
			for _, group := range owned {
				if inUse(group) {
					ordered = append(ordered, group)
				}
			}
			// Keep an owned live pipeline before reviving an inactive cached match.
			ordered = append(ordered, selected)
			ordered = append(ordered, owned...)
			for slot := 0; slot < len(source.outputBitrates); slot++ {
				if group := source.groups[slot]; group != nil {
					ordered = append(ordered, group)
				}
			}
			for _, group := range ordered {
				if group == nil {
					continue
				}
				if source.outputBitrates[group.slot] == 0 {
					continue
				}
				if compatible(group, demand) {
					selected = group
					break
				}
			}
		}
		if selected == nil {
			for slot := 2; slot < len(source.outputBitrates); slot++ {
				if source.groups[slot] == nil && source.outputBitrates[slot] > 0 {
					var err error
					selected, err = source.newGroup(slot)
					if err != nil {
						return plan, err
					}
					break
				}
			}
		}
		if selected == nil {
			// Existing delivery remains while a prepared split releases its old
			// membership. No timer, retry queue or temporary capacity exemption.
			continue
		}
		if selected.budget != demand.budget || source.memberships[demand.consumer] != selected {
			if slog.Default().Enabled(source.engine.ctx, slog.LevelDebug) {
				slog.Debug("encoding-group", "event", "demand", "slot", selected.slot, "budget", demand.budget,
					"sourceId", diagnostics.ID(fmt.Sprintf("%p", source)), "rtcPeerId", groupConsumerID(demand.consumer))
			}
		}
		selected.budget = demand.budget
		if !selected.active {
			selected.start = source.inputPTS
		}
		if source.memberships[demand.consumer] != selected && demand.consumer.CurrentSource() != selected.media.Source {
			source.groupRecovery.Or(uint32(1) << selected.slot)
		}
		source.memberships[demand.consumer] = selected
	}
	// The full-resolution encoder follows its slowest viewer down to half its
	// ceiling; below that a viewer moves to the derived resolution instead.
	if top := source.outputBitrates[1]; top > 0 {
		follow := top
		divisor := uint64(max(2, source.floorDivisor))
		for _, demand := range demands {
			// A viewer on the derived copy still bounds the full-size encoder:
			// raising it to the ceiling then would keep that viewer from returning.
			if demand.active && demand.full < follow && divisor*uint64(demand.full) >= uint64(top) {
				follow = demand.full
			}
		}
		step := max(int64(top)/100, 1000)
		codec := min(source.media.CodecBudget(1, int64(follow)), int64(top))
		plan.Bitrates[1] = uint32(max(int64(top)/int64(divisor), codec/step*step))
	}
	for _, group := range source.groups {
		active := group.slot == 0 && plan.Active[1] && !hasLowerDemand
		for _, demand := range demands {
			if !demand.active {
				continue
			}
			if source.memberships[demand.consumer] == group ||
				demand.consumer.CurrentSource() == group.media.Source && (demand.lower || !demand.original || group.slot != 0) {
				active = true
			}
		}
		nextActive := active && source.outputBitrates[group.slot] > 0
		if group.active != nextActive {
			slog.Debug("encoding-group", "event", "activity", "slot", group.slot, "active", nextActive, "sourceId", diagnostics.ID(fmt.Sprintf("%p", source)))
		}
		group.active = nextActive
		plan.Active[group.slot] = group.active
		if group.active {
			// Measured packet overhead wobbles per frame; 1% steps keep the
			// encoder target still instead of rewriting it every frame.
			ceiling := int64(source.outputBitrates[group.slot])
			step := max(ceiling/100, 1000)
			codec := min(group.media.CodecBudget(0, int64(group.budget)), ceiling)
			plan.Bitrates[group.slot] = uint32(max(1000, codec/step*step))
		}
	}
	if source.tracePlans {
		if line, changed := source.planTrace.describe(plan, demands); changed {
			slog.Debug("encoder-slots", "plan", line, "sourceId", diagnostics.ID(fmt.Sprintf("%p", source)))
		}
	}
	return plan, nil
}

func (source *Source) newGroup(slot int) (*outputGroup, error) {
	info := source.media.TrackInfo()
	formats := make([]forwarding.LayerFormat, len(info.Layers))
	for layer, format := range info.Layers {
		formats[layer] = forwarding.LayerFormat{Width: format.Width, Height: format.Height, Bitrate: format.Bitrate}
	}
	formats[0].Bitrate = source.outputBitrates[slot]
	media, err := forwarding.NewEncodedSource(forwarding.SourceOptions{
		ID: string(source.media.TrackID()), StreamID: source.media.StreamID(), Codec: videoCodecs[source.codec],
		Formats: formats, MaxPackets: encoded.MaxPacketWindow,
		OnRTCP: func(layer int, packets []rtcp.Packet) {
			for _, packet := range packets {
				switch packet.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					if layer == 0 {
						source.requestLayerKeyFrame(slot)
					} else {
						source.requestLayerKeyFrame(1)
					}
				}
			}
		},
	})
	if err != nil {
		return nil, err
	}
	media.SetLowestLayerRateControlled(true)
	media.SetTopLayerFloorDivisor(source.floorDivisor)
	group := &outputGroup{slot: slot, media: media, start: source.inputPTS}
	if source.hasInput {
		if err = media.BeginFrame(source.inputPTS, source.inputAt); err != nil {
			media.Close()
			return nil, err
		}
	}
	source.groups[slot] = group
	return group, nil
}

func (source *Source) installGroup(group *outputGroup, original bool) {
	next := source.media.Source
	if group != nil {
		next = group.media.Source
	}
	for consumer, wanted := range source.memberships {
		if wanted == group && (original || group != nil) && consumer.CurrentSource() != next {
			err := consumer.ReplaceSource(next)
			if slog.Default().Enabled(source.engine.ctx, slog.LevelDebug) {
				slot := -1
				if group != nil {
					slot = group.slot
				}
				slog.Debug("encoding-group", "event", "attachment", "slot", slot, "original", original,
					"sourceId", diagnostics.ID(fmt.Sprintf("%p", source)), "rtcPeerId", groupConsumerID(consumer), diagnostics.Error(err))
			}
		}
	}
}

func groupConsumerID(consumer groupConsumer) string {
	switch consumer := consumer.(type) {
	case *forwarding.Transport:
		return diagnostics.ID(consumer.PC.ID())
	case *forwarding.Publication:
		return diagnostics.ID(consumer.PC.ID())
	}
	return ""
}

func (source *Source) beginGroupFrame(pts time.Duration, at time.Time) error {
	if err := source.media.BeginFrame(pts, at); err != nil {
		return err
	}
	source.inputPTS, source.inputAt, source.hasInput = pts, at, true
	for slot, group := range source.groups {
		if slot != 0 {
			if err := group.media.BeginFrame(pts, at); err != nil {
				return err
			}
		}
	}
	return nil
}
