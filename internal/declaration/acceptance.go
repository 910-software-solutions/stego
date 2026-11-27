package declaration

import "time"

// acceptanceDate is the form of the date of an experimental acceptance and of the sunset of a component
// (ST-DECL-EN07).
const acceptanceDate = "2006-01-02"

// AcceptanceOutlivesSunset enforces ST-DECL-INV15: an acceptance date must not be later than the sunset date of the
// component it accepts. It gives true when until, the accepted_until of the acceptance, and sunset, the sunset that
// the registry states for the component, are both dates and until is later than sunset. An acceptance until the
// sunset day itself holds. A date that is not a date is a fault of another rule, so it gives false here.
func AcceptanceOutlivesSunset(until, sunset string) bool {
	accepted, err := time.Parse(acceptanceDate, until)
	if err != nil {
		return false
	}
	end, err := time.Parse(acceptanceDate, sunset)
	if err != nil {
		return false
	}
	return accepted.After(end)
}
