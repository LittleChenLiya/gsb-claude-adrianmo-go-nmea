package nmea_test

import (
	"fmt"

	nmea "github.com/adrianmo/go-nmea"
)

// ExampleParse is a short regression example for the original Parse interface:
// known fields keep their original parsed values, and unknown trailing fields
// of a variable-length sentence (RMC here) are exposed via the extension view
// with their raw values and positions.
func ExampleParse() {
	// classic sentence: parsed exactly as before, no extension fields
	s, err := nmea.Parse("$GNRMC,220516,A,5133.82,N,00042.24,W,173.8,231.8,130694,004.2,W*6E")
	if err != nil {
		panic(err)
	}
	rmc := s.(nmea.RMC)
	fmt.Printf("validity=%s lat=%.4f lon=%.4f speed=%.1f extensions=%d\n",
		rmc.Validity, rmc.Latitude, rmc.Longitude, rmc.Speed, len(rmc.Extensions))

	// same sentence with two unknown trailing fields appended
	s, err = nmea.Parse("$GNRMC,102014.00,A,5550.6082,N,03732.2488,E,000.00000,092.9,300518,,,A,V,EXT1,EXT2*38")
	if err != nil {
		panic(err)
	}
	rmc = s.(nmea.RMC)
	fmt.Printf("validity=%s navStatus=%s course=%.1f\n", rmc.Validity, rmc.NavStatus, rmc.Course)
	for _, ext := range rmc.Extensions {
		fmt.Printf("extension[%d]=%q\n", ext.Index, ext.Value)
	}

	// Output:
	// validity=A lat=51.5637 lon=-0.7040 speed=173.8 extensions=0
	// validity=A navStatus=V course=92.9
	// extension[13]="EXT1"
	// extension[14]="EXT2"
}
