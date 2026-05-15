package coordinator

import (
	"math/rand"
	"sync"
)

// modelRegistry tracks which DCS ship unit types are actually available in the
// running DCS installation. If the hook reports models, only those are used.
// If no report has been received yet, all models are assumed available.
var (
	modelMu     sync.RWMutex
	modelSet    map[string]bool // nil = all available
	modelLoaded bool
)

// SetAvailableModels stores the set of ship unit types reported by the DCS hook.
func SetAvailableModels(models []string) {
	modelMu.Lock()
	defer modelMu.Unlock()
	modelSet = make(map[string]bool, len(models))
	for _, m := range models {
		modelSet[m] = true
	}
	modelLoaded = true
}

// isAvailable checks whether a model name is usable in DCS. Returns true if
// no model report has been received yet (assume everything is available).
func isAvailable(name string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	if !modelLoaded {
		return true
	}
	return modelSet[name]
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

// DCSUnitType returns a DCS unit type name for the given AIS ship, using both
// the AIS type code and actual vessel length (metres) to pick the closest match.
// Brackets use midpoint thresholds between adjacent model sizes.
//
// Actual DCS model lengths (from Lua definitions):
//
//	yacht_ship                    20 m   (CAP Navy)
//	HarborTug                    20 m   (DCS SouthAtlanticAssets)
//	diesel_trawler               38 m   (CAP Navy)
//	yacht_helipad                40 m   (CAP Navy)
//	La_Combattante_II            46 m   (DCS TechWeaponPack)
//	fishing_vessel               50 m   (CAP Navy)
//	trawler_ship                 65 m   (CAP Navy)
//	jr_more_tug                  65 m   (CAP Navy)
//	jr_more_tug_helipad          65 m   (CAP Navy)
//	ALBATROS                     71 m   (Currenthill Assets Pack)
//	CastleClass_01               74 m   (DCS SouthAtlanticAssets)
//	CHAP_Project22160            94 m   (Currenthill Assets Pack)
//	old_vessel                  100 m   (CAP Navy)
//	leander-gun-ariadne         110 m   (DCS SouthAtlanticAssets)
//	BDK-775                     112 m   (DCS TechWeaponPack)
//	akademik_cherskiy           116 m   (CAP Navy)
//	akademik_cherskiy_pipe_laying 116 m (CAP Navy)
//	kimedaka_skyicher           116 m   (CAP Navy)
//	container_ship              180 m   (CAP Navy)
//	HandyWind                   180 m   (DCS default)
//	Ship_Tilde_Supply           180 m   (DCS default)
//	ievoli_ivory                190 m   (CAP Navy)
//	lng_tanker                  245 m   (CAP Navy)
//	Seawise_Giant               457 m   (DCS default)
func DCSUnitType(aisType int, length int) string {
	category := ShipCategory(aisType)

	switch category {
	case "fishing":
		return fishingByLength(length)
	case "tug":
		return tugByLength(length)
	case "pleasure":
		return pleasureByLength(length)
	case "passenger":
		return passengerByLength(length)
	case "cargo":
		return cargoByLength(length)
	case "tanker":
		return tankerByLength(length)
	default:
		return otherByLength(length)
	}
}

func fishingByLength(length int) string {
	switch {
	case length > 57: // midpoint between fishing_vessel(50) and trawler_ship(65)
		return pick("trawler_ship")
	case length > 44: // midpoint between diesel_trawler(38) and fishing_vessel(50)
		return pick("fishing_vessel")
	case length > 0:
		return pick("diesel_trawler")
	default: // unknown length
		return pick("fishing_vessel", "trawler_ship", "diesel_trawler")
	}
}

func tugByLength(length int) string {
	switch {
	case length > 42: // midpoint between HarborTug(20) and jr_more_tug(65)
		return pick("jr_more_tug", "jr_more_tug_helipad")
	case length > 0:
		return pick("HarborTug")
	default: // unknown length
		return pick("jr_more_tug", "jr_more_tug_helipad", "HarborTug")
	}
}

func pleasureByLength(length int) string {
	switch {
	case length > 30: // midpoint between yacht_ship(20) and yacht_helipad(40)
		return pick("yacht_helipad")
	default:
		return pick("yacht_ship", "yacht_helipad")
	}
}

func passengerByLength(length int) string {
	switch {
	case length > 148: // large passenger — use big civilian hulls
		return pick("container_ship", "HandyWind")
	case length > 82: // medium passenger
		return pick("container_ship", "HandyWind")
	case length > 30: // small passenger — yacht-sized
		return pick("yacht_helipad")
	default:
		return pick("yacht_ship", "yacht_helipad")
	}
}

func cargoByLength(length int) string {
	switch {
	case length > 148: // large cargo — actual cargo ship models
		return pick("container_ship", "HandyWind")
	case length > 82: // medium cargo — still use cargo ships (slightly oversized but correct type)
		return pick("container_ship", "HandyWind")
	case length > 0: // small cargo — generic vessel as best available
		return pick("old_vessel")
	default: // unknown length
		return pick("container_ship", "HandyWind")
	}
}

func tankerByLength(length int) string {
	switch {
	case length > 351:
		return pick("Seawise_Giant")
	case length > 217:
		return pick("lng_tanker")
	case length > 145: // midpoint between old_vessel(100) and ievoli_ivory(190)
		return pick("ievoli_ivory")
	case length > 0:
		return pick("old_vessel")
	default: // unknown length -- use the tanker-shaped model
		return pick("ievoli_ivory", "lng_tanker")
	}
}

func otherByLength(length int) string {
	switch {
	case length > 351:
		return pick("Seawise_Giant")
	case length > 217:
		return pick("lng_tanker")
	case length > 148:
		return pick("container_ship", "HandyWind", "Ship_Tilde_Supply")
	case length > 105: // leander(110), BDK-775(112), akademik(116)
		return pick("akademik_cherskiy", "kimedaka_skyicher", "akademik_cherskiy_pipe_laying",
			"leander-gun-ariadne", "BDK-775")
	case length > 84: // CHAP_Project22160(94), old_vessel(100)
		return pick("old_vessel", "CHAP_Project22160")
	case length > 68: // ALBATROS(71), CastleClass_01(74)
		return pick("ALBATROS", "CastleClass_01")
	case length > 42: // La_Combattante_II(46), trawler_ship(65)
		return pick("trawler_ship", "La_Combattante_II")
	case length > 0:
		return pick("diesel_trawler", "HarborTug")
	default: // unknown
		return pick("old_vessel", "Ship_Tilde_Supply")
	}
}

// pick selects a random model from the choices, filtering to only those that
// are reported as available in DCS. Falls back to universal defaults if none
// of the preferred models are installed.
func pick(choices ...string) string {
	var available []string
	for _, c := range choices {
		if isAvailable(c) {
			available = append(available, c)
		}
	}
	if len(available) == 0 {
		// None of the preferred models are installed. Try universal fallbacks.
		for _, fb := range []string{"Ship_Tilde_Supply", "old_vessel", "HandyWind"} {
			if isAvailable(fb) {
				return fb
			}
		}
		return choices[0]
	}
	return available[rand.Intn(len(available))]
}
