package sim

import (
	"math/rand"
	"time"
)

// FleetConfig describes a simulated fleet.
type FleetConfig struct {
	Vehicles int
	Seed     int64
	Start    time.Time
	// FaultRate is the fraction of vehicles that receive a fault. Faults are spread
	// evenly across types so that small fleets still exercise each one.
	FaultRate float64
	// MaxOnset bounds how far after Start a fault may begin.
	MaxOnset time.Duration
}

// NewFleet builds a deterministic fleet. The same config yields the same vehicles
// and faults, which keeps evaluation runs reproducible.
func NewFleet(cfg FleetConfig) []*Vehicle {
	rng := rand.New(rand.NewSource(cfg.Seed))
	faulty := int(float64(cfg.Vehicles) * cfg.FaultRate)
	assignments := make([]Fault, cfg.Vehicles)
	for i := 0; i < faulty; i++ {
		assignments[i] = AllFaults[i%len(AllFaults)]
	}
	rng.Shuffle(len(assignments), func(i, j int) {
		assignments[i], assignments[j] = assignments[j], assignments[i]
	})
	fleet := make([]*Vehicle, cfg.Vehicles)
	for i := range fleet {
		onset := cfg.Start
		if cfg.MaxOnset > 0 {
			onset = cfg.Start.Add(time.Duration(rng.Int63n(int64(cfg.MaxOnset))))
		}
		fleet[i] = NewVehicle(i, rng.Int63(), cfg.Start, assignments[i], onset)
	}
	return fleet
}
