package coordinator

import (
	"math/rand"
	"sync"
)

// modelRegistry tracks which DCS ship unit types are actually available in the
// running DCS installation. Each coordinator has its own registry since different
// DCS instances may have different mods installed.
type modelRegistry struct {
	mu     sync.RWMutex
	models map[string]bool // nil = all available
	loaded bool
}

// setAvailableModels stores the set of ship unit types reported by the DCS hook.
func (r *modelRegistry) setAvailableModels(models []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = make(map[string]bool, len(models))
	for _, m := range models {
		r.models[m] = true
	}
	r.loaded = true
}

// isAvailable checks whether a model name is usable in DCS. Returns true if
// no model report has been received yet (assume everything is available).
func (r *modelRegistry) isAvailable(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.loaded {
		return true
	}
	return r.models[name]
}

// count returns the number of loaded models, or 0 if not yet loaded.
func (r *modelRegistry) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.models)
}

// ShipCategory classifies an AIS ship type code into a filter category.
func ShipCategory(aisType int) string {
	switch {
	case aisType == 30:
		return "fishing"
	case aisType >= 31 && aisType <= 32:
		return "tug"
	case aisType == 52:
		return "tug"
	case aisType >= 36 && aisType <= 37:
		return "pleasure"
	case aisType >= 60 && aisType <= 69:
		return "passenger"
	case aisType >= 70 && aisType <= 79:
		return "cargo"
	case aisType >= 80 && aisType <= 89:
		return "tanker"
	default:
		return "other"
	}
}

// dcsUnitType returns a DCS unit type name for the given AIS ship, using both
// the AIS type code and actual vessel length (metres) to pick the closest match.
func (r *modelRegistry) dcsUnitType(aisType int, length int) string {
	category := ShipCategory(aisType)

	switch category {
	case "fishing":
		return r.fishingByLength(length)
	case "tug":
		return r.tugByLength(length)
	case "pleasure":
		return r.pleasureByLength(length)
	case "passenger":
		return r.passengerByLength(length)
	case "cargo":
		return r.cargoByLength(length)
	case "tanker":
		return r.tankerByLength(length)
	default:
		return r.otherByLength(length)
	}
}

func (r *modelRegistry) fishingByLength(length int) string {
	switch {
	case length > 57:
		return r.pick("trawler_ship")
	case length > 44:
		return r.pick("fishing_vessel")
	case length > 0:
		return r.pick("diesel_trawler")
	default:
		return r.pick("fishing_vessel", "trawler_ship", "diesel_trawler")
	}
}

func (r *modelRegistry) tugByLength(length int) string {
	switch {
	case length > 42:
		return r.pick("jr_more_tug", "jr_more_tug_helipad")
	case length > 0:
		return r.pick("HarborTug")
	default:
		return r.pick("jr_more_tug", "jr_more_tug_helipad", "HarborTug")
	}
}

func (r *modelRegistry) pleasureByLength(length int) string {
	switch {
	case length > 30:
		return r.pick("yacht_helipad")
	default:
		return r.pick("yacht_ship", "yacht_helipad")
	}
}

func (r *modelRegistry) passengerByLength(length int) string {
	switch {
	case length > 148:
		return r.pick("container_ship", "HandyWind")
	case length > 82:
		return r.pick("container_ship", "HandyWind")
	case length > 30:
		return r.pick("yacht_helipad")
	default:
		return r.pick("yacht_ship", "yacht_helipad")
	}
}

func (r *modelRegistry) cargoByLength(length int) string {
	switch {
	case length > 148:
		return r.pick("container_ship", "HandyWind")
	case length > 82:
		return r.pick("container_ship", "HandyWind")
	case length > 0:
		return r.pick("old_vessel")
	default:
		return r.pick("container_ship", "HandyWind")
	}
}

func (r *modelRegistry) tankerByLength(length int) string {
	switch {
	case length > 351:
		return r.pick("Seawise_Giant")
	case length > 217:
		return r.pick("lng_tanker")
	case length > 145:
		return r.pick("ievoli_ivory")
	case length > 0:
		return r.pick("old_vessel")
	default:
		return r.pick("ievoli_ivory", "lng_tanker")
	}
}

func (r *modelRegistry) otherByLength(length int) string {
	switch {
	case length > 351:
		return r.pick("Seawise_Giant")
	case length > 217:
		return r.pick("lng_tanker")
	case length > 148:
		return r.pick("container_ship", "HandyWind", "Ship_Tilde_Supply")
	case length > 105:
		return r.pick("akademik_cherskiy", "kimedaka_skyicher", "akademik_cherskiy_pipe_laying",
			"leander-gun-ariadne", "BDK-775")
	case length > 84:
		return r.pick("old_vessel", "CHAP_Project22160")
	case length > 68:
		return r.pick("ALBATROS", "CastleClass_01")
	case length > 42:
		return r.pick("trawler_ship", "La_Combattante_II")
	case length > 0:
		return r.pick("diesel_trawler", "HarborTug")
	default:
		return r.pick("old_vessel", "Ship_Tilde_Supply")
	}
}

// pick selects a random model from the choices, filtering to only those that
// are reported as available in DCS. Falls back to universal defaults if none
// of the preferred models are installed.
//
// Acquires the lock once and checks all candidates under a single snapshot
// to avoid per-candidate lock/unlock overhead on the hot AIS path.
func (r *modelRegistry) pick(choices ...string) string {
	r.mu.RLock()
	loaded := r.loaded
	models := r.models
	r.mu.RUnlock()

	isAvail := func(name string) bool {
		if !loaded {
			return true
		}
		return models[name]
	}

	var available []string
	for _, c := range choices {
		if isAvail(c) {
			available = append(available, c)
		}
	}
	if len(available) == 0 {
		for _, fb := range []string{"Ship_Tilde_Supply", "old_vessel", "HandyWind"} {
			if isAvail(fb) {
				return fb
			}
		}
		return choices[0]
	}
	return available[rand.Intn(len(available))]
}
