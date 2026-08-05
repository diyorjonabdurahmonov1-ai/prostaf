// Package geo provides geographic helpers.
package geo

import "math"

const earthRadiusKm = 6371.0

// DistanceKm returns the great-circle distance in kilometres between two
// (lat, lng) points using the Haversine formula.
func DistanceKm(lat1, lng1, lat2, lng2 float64) float64 {
	dLat := rad(lat2 - lat1)
	dLng := rad(lng2 - lng1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(lat1))*math.Cos(rad(lat2))*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusKm * c
}

func rad(deg float64) float64 { return deg * math.Pi / 180 }
