// Package ecobee implements the read-only subset of the documented ecobee v1 API.
package ecobee

type Thermostat struct {
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
	Settings   struct {
		HvacMode string `json:"hvacMode"`
	} `json:"settings"`
	Runtime struct {
		Connected         bool `json:"connected"`
		ActualTemperature *int `json:"actualTemperature"`
		DesiredHeat       *int `json:"desiredHeat"`
		DesiredCool       *int `json:"desiredCool"`
	} `json:"runtime"`
	ExtendedRuntime struct {
		HvacMode     []string `json:"hvacMode"`
		HeatPump1    []int    `json:"heatPump1"`
		HeatPump2    []int    `json:"heatPump2"`
		AuxHeat1     []int    `json:"auxHeat1"`
		AuxHeat2     []int    `json:"auxHeat2"`
		Cool1        []int    `json:"cool1"`
		Cool2        []int    `json:"cool2"`
		Fan          []int    `json:"fan"`
		Humidifier   []int    `json:"humidifier"`
		Dehumidifier []int    `json:"dehumidifier"`
		Economizer   []int    `json:"economizer"`
		Ventilator   []int    `json:"ventilator"`
	} `json:"extendedRuntime"`
	RemoteSensors []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Type       string `json:"type"`
		InUse      bool   `json:"inUse"`
		Capability []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"capability"`
	} `json:"remoteSensors"`
}
