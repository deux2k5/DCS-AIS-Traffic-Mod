package geo

import "math"

// BoundingBox defines a geographic rectangle by its south-west and north-east
// corners as [lat, lon] pairs.
type BoundingBox struct {
	SW [2]float64 // {lat, lon}
	NE [2]float64 // {lat, lon}
}

// TheatreBounds maps DCS theatre names to their AIS bounding boxes.
// Boxes are deliberately wider than the DCS map to capture ships approaching
// the theatre edges. The coordinator's Contains check trims to the playable area.
var TheatreBounds = map[string]BoundingBox{
	"Caucasus":       {SW: [2]float64{38.0, 27.0}, NE: [2]float64{48.0, 46.0}},  // full Black Sea incl. Istanbul/Bosporus
	"PersianGulf":    {SW: [2]float64{20.0, 44.0}, NE: [2]float64{32.0, 60.0}},  // wider Gulf + Gulf of Oman
	"Syria":          {SW: [2]float64{30.0, 29.0}, NE: [2]float64{40.0, 40.0}},  // eastern Med + Suez approach
	"SinaiMap":       {SW: [2]float64{25.0, 30.0}, NE: [2]float64{35.0, 38.0}},  // Red Sea north + east Med
	"Falklands":      {SW: [2]float64{-57.0, -68.0}, NE: [2]float64{-46.0, -52.0}}, // wider South Atlantic
	"MarianaIslands": {SW: [2]float64{10.0, 141.0}, NE: [2]float64{18.0, 149.0}}, // wider Pacific around Guam
	"Kola":           {SW: [2]float64{65.0, 24.0}, NE: [2]float64{74.0, 44.0}},  // Barents Sea + Norwegian coast
}

// Contains reports whether the given lat/lon is inside the bounding box.
func (bb BoundingBox) Contains(lat, lon float64) bool {
	latOK := lat >= bb.SW[0] && lat <= bb.NE[0]
	lonOK := lon >= bb.SW[1] && lon <= bb.NE[1]
	return latOK && lonOK
}

// AISBox returns the bounding box formatted for the aisstream.io subscribe
// message: [[[lat1, lon1], [lat2, lon2]]].
func (bb BoundingBox) AISBox() [][2]float64 {
	return [][2]float64{bb.SW, bb.NE}
}

// TheatreNames returns a sorted list of available theatre names.
func TheatreNames() []string {
	return []string{
		"Caucasus",
		"Falklands",
		"Kola",
		"MarianaIslands",
		"PersianGulf",
		"SinaiMap",
		"Syria",
	}
}

// EquirectangularDistance is a fast approximate distance in metres between two
// lat/lon pairs. Uses a simple equirectangular projection which is accurate
// enough for short distances (< 10 km) and much cheaper than haversine since
// it avoids sin/cos/atan2/sqrt beyond one cosine call.
func EquirectangularDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const metersPerDeg = 111320.0
	dLat := (lat2 - lat1) * metersPerDeg
	dLon := (lon2 - lon1) * metersPerDeg * math.Cos(degreesToRadians((lat1+lat2)/2))
	return math.Sqrt(dLat*dLat + dLon*dLon)
}

// HaversineDistance computes the distance in metres between two lat/lon pairs.
func HaversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371000.0 // metres

	dLat := degreesToRadians(lat2 - lat1)
	dLon := degreesToRadians(lon2 - lon1)

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(degreesToRadians(lat1))*math.Cos(degreesToRadians(lat2))*
			math.Sin(dLon/2)*math.Sin(dLon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadius * c
}

func degreesToRadians(d float64) float64 {
	return d * math.Pi / 180.0
}
