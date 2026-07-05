package format

import "testing"

// tronFromJSON decodes src through the real IR pipeline and TRON-encodes it.
func tronFromJSON(t *testing.T, src string) string {
	t.Helper()
	v, ok, err := decodeAll([]byte(src))
	if err != nil || !ok {
		t.Fatalf("decodeAll(%q): ok=%v err=%v", src, ok, err)
	}
	out, err := encodeTRON(v)
	if err != nil {
		t.Fatalf("encodeTRON: %v", err)
	}
	return out
}

// TestTronGoldens pins encodeTRON byte-for-byte against the JS reference
// implementation (tron-format, commit d34e610): every want below was minted by
// running src/stringify.ts on the same input, except the two number cases at
// the end, which pin this port's verbatim-decimal divergence.
func TestTronGoldens(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"integer", `123`, `123`},
		{"string", `"hello"`, `"hello"`},
		{"true", `true`, `true`},
		{"false", `false`, `false`},
		{"null", `null`, `null`},
		{"int array", `[1,2,3]`, `[1,2,3]`},
		{"string array", `["a","b"]`, `["a","b"]`},
		{"empty object", `{}`, `{}`},
		{
			"class reuse",
			`[{"x":1,"y":2,"z":3},{"x":3,"y":4,"z":5}]`,
			"class A: x,y,z\n\n[A(1,2,3),A(3,4,5)]",
		},
		{
			"ndjson stream folds to array",
			"{\"x\":1,\"y\":2}\n{\"x\":3,\"y\":4}",
			"class A: x,y\n\n[A(1,2),A(3,4)]",
		},
		{
			"instance values reordered to declaration order",
			`{"a":{"x":1,"y":2,"z":3},"b":{"y":4,"x":3,"z":5},"c":{"x":2,"y":4,"z":6}}`,
			"class A: x,y,z\n\n{\"a\":A(1,2,3),\"b\":A(3,4,5),\"c\":A(2,4,6)}",
		},
		{
			"non-identifier declaration keys quoted",
			`[{"1a":1,"a1":2,"valid_name":3,"foo-bar":4},{"1a":1,"a1":2,"valid_name":3,"foo-bar":4}]`,
			"class A: \"1a\",a1,valid_name,\"foo-bar\"\n\n[A(1,2,3,4),A(1,2,3,4)]",
		},
		{
			"single occurrence stays JSON",
			`{"x":1,"y":2,"z":3}`,
			`{"x":1,"y":2,"z":3}`,
		},
		{
			"single property stays JSON despite repeats",
			`[{"x":1},{"x":2},{"x":3}]`,
			`[{"x":1},{"x":2},{"x":3}]`,
		},
		{
			"two properties twice mints a class",
			`[{"x":1,"y":2},{"x":3,"y":4}]`,
			"class A: x,y\n\n[A(1,2),A(3,4)]",
		},
		{
			"mixed qualifying and non-qualifying shapes",
			`{"single":{"a":1},"oneTwice":[{"b":2},{"b":3}],"twoTwice":[{"d":4,"e":5},{"d":6,"e":7}],"threeOnce":{"f":8,"g":9,"h":10}}`,
			"class A: d,e\n\n{\"single\":{\"a\":1},\"oneTwice\":[{\"b\":2},{\"b\":3}],\"twoTwice\":[A(4,5),A(6,7)],\"threeOnce\":{\"f\":8,\"g\":9,\"h\":10}}",
		},
		{
			"nested classes in DFS pre-order",
			`[{"p":{"m":1,"n":2},"q":3},{"p":{"m":4,"n":5},"q":6}]`,
			"class A: p,q\nclass B: m,n\n\n[A(B(1,2),3),A(B(4,5),6)]",
		},
		{
			"names assigned by discovery order not qualification order",
			`[{"x":1,"y":2},{"m":1,"n":2},{"m":3,"n":4},{"x":3,"y":4}]`,
			"class A: x,y\nclass B: m,n\n\n[A(1,2),B(1,2),B(3,4),A(3,4)]",
		},
		{
			"roundtrip users and meta",
			`{"users":[{"id":1,"name":"Alice","active":true},{"id":2,"name":"Bob","active":false}],"meta":{"page":1,"total":2}}`,
			"class A: id,name,active\n\n{\"users\":[A(1,\"Alice\",true),A(2,\"Bob\",false)],\"meta\":{\"page\":1,\"total\":2}}",
		},
		{
			"strings JSON-escaped without HTML escaping",
			`{"s":"<a>&\n\"q\"\t\\"}`,
			"{\"s\":\"<a>&\\n\\\"q\\\"\\t\\\\\"}",
		},
		{
			"empty objects never fingerprinted",
			`{"a":{},"b":{}}`,
			`{"a":{},"b":{}}`,
		},
		{
			"null instance values",
			`[{"x":null,"y":1},{"x":2,"y":null}]`,
			"class A: x,y\n\n[A(null,1),A(2,null)]",
		},
		{
			// Divergence from JS (Number would round-trip through float64):
			// a 21-digit integer stays digit-exact via *big.Int.
			"big integer digit-exact",
			`[{"id":123456789012345678901,"v":1},{"id":2,"v":3}]`,
			"class A: id,v\n\n[A(123456789012345678901,1),A(2,3)]",
		},
		{
			// Divergence from JS (String(53.0) === "53"): non-integer numbers
			// pass through as verbatim json.Number decimal text.
			"decimal passthrough verbatim",
			`[{"a":1.5,"b":2.25},{"a":0.1,"b":53.0}]`,
			"class A: a,b\n\n[A(1.5,2.25),A(0.1,53.0)]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tronFromJSON(t, tt.src); got != tt.want {
				t.Errorf("encodeTRON(%q) = %q, want %q", tt.src, got, tt.want)
			}
		})
	}
}

// TestTronFingerprintNoCommaCollision pins the deliberate divergence from the
// JS reference: fingerprints join sorted keys with NUL, not ",", so
// {"a,b","c"} and {"a","b,c"} are distinct key-sets. The JS impl merges them
// into one class and corrupts the second shape to A(null,null).
func TestTronFingerprintNoCommaCollision(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			"one occurrence each mints nothing",
			`[{"a,b":1,"c":2},{"a":1,"b,c":2}]`,
			`[{"a,b":1,"c":2},{"a":1,"b,c":2}]`,
		},
		{
			"two occurrences each mint distinct classes",
			`[{"a,b":1,"c":2},{"a,b":3,"c":4},{"a":5,"b,c":6},{"a":7,"b,c":8}]`,
			"class A: \"a,b\",c\nclass B: a,\"b,c\"\n\n[A(1,2),A(3,4),B(5,6),B(7,8)]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tronFromJSON(t, tt.src); got != tt.want {
				t.Errorf("encodeTRON(%q) = %q, want %q", tt.src, got, tt.want)
			}
		})
	}
}

func TestTronClassName(t *testing.T) {
	tests := []struct {
		index int
		want  string
	}{
		{0, "A"},
		{1, "B"},
		{25, "Z"},
		{26, "A1"},
		{27, "B1"},
		{51, "Z1"},
		{52, "A2"},
		{77, "Z2"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tronClassName(tt.index); got != tt.want {
				t.Errorf("tronClassName(%d) = %q, want %q", tt.index, got, tt.want)
			}
		})
	}
}

// tronWeatherJSON is examples/weather_api_response/json_compact.json from the
// JS reference repo; tronWeatherWant is its tron_token_efficient.tron, which
// stringify.ts reproduces byte-for-byte from this input.
const (
	tronWeatherJSON = `{"bulk":[{"query":{"custom_id":"my-id-1","q":"53,-0.12","location":{"name":"Boston","region":"Lincolnshire","country":"United Kingdom","lat":53,"lon":-0.12,"tz_id":"Europe/London","localtime_epoch":1673620218,"localtime":"2023-01-13 14:30"},"current":{"last_updated_epoch":1673620200,"last_updated":"2023-01-13 14:30","temp_c":8.7,"temp_f":47.7,"is_day":1,"condition":{"text":"Partly cloudy","icon":"//cdn.weatherapi.com/weather/64x64/day/116.png","code":1003},"wind_mph":24.2,"wind_kph":38.9,"wind_degree":260,"wind_dir":"W","pressure_mb":1005,"pressure_in":29.68,"precip_mm":0,"precip_in":0,"humidity":74,"cloud":75,"feelslike_c":4.4,"feelslike_f":39.9,"vis_km":10,"vis_miles":6,"uv":2,"gust_mph":33.1,"gust_kph":53.3}}},{"query":{"custom_id":"any-internal-id","q":"London","location":{"name":"London","region":"City of London, Greater London","country":"United Kingdom","lat":51.52,"lon":-0.11,"tz_id":"Europe/London","localtime_epoch":1673620218,"localtime":"2023-01-13 14:30"},"current":{"last_updated_epoch":1673620200,"last_updated":"2023-01-13 14:30","temp_c":11,"temp_f":51.8,"is_day":1,"condition":{"text":"Partly cloudy","icon":"//cdn.weatherapi.com/weather/64x64/day/116.png","code":1003},"wind_mph":23,"wind_kph":37.1,"wind_degree":270,"wind_dir":"W","pressure_mb":1010,"pressure_in":29.83,"precip_mm":0,"precip_in":0,"humidity":58,"cloud":75,"feelslike_c":8.1,"feelslike_f":46.5,"vis_km":10,"vis_miles":6,"uv":2,"gust_mph":22.4,"gust_kph":36}}},{"query":{"custom_id":"us-zipcode-id-765","q":"90201","location":{"name":"Bell","region":"California","country":"USA","lat":33.97,"lon":-118.17,"tz_id":"America/Los_Angeles","localtime_epoch":1673620220,"localtime":"2023-01-13 6:30"},"current":{"last_updated_epoch":1673620200,"last_updated":"2023-01-13 06:30","temp_c":10,"temp_f":50,"is_day":0,"condition":{"text":"Clear","icon":"//cdn.weatherapi.com/weather/64x64/night/113.png","code":1000},"wind_mph":2.2,"wind_kph":3.6,"wind_degree":10,"wind_dir":"N","pressure_mb":1020,"pressure_in":30.13,"precip_mm":0,"precip_in":0,"humidity":74,"cloud":0,"feelslike_c":10.3,"feelslike_f":50.5,"vis_km":16,"vis_miles":9,"uv":1,"gust_mph":3.6,"gust_kph":5.8}}}]}`

	tronWeatherWant = `class A: custom_id,q,location,current
class B: name,region,country,lat,lon,tz_id,localtime_epoch,localtime
class C: last_updated_epoch,last_updated,temp_c,temp_f,is_day,condition,wind_mph,wind_kph,wind_degree,wind_dir,pressure_mb,pressure_in,precip_mm,precip_in,humidity,cloud,feelslike_c,feelslike_f,vis_km,vis_miles,uv,gust_mph,gust_kph
class D: text,icon,code

{"bulk":[{"query":A("my-id-1","53,-0.12",B("Boston","Lincolnshire","United Kingdom",53,-0.12,"Europe/London",1673620218,"2023-01-13 14:30"),C(1673620200,"2023-01-13 14:30",8.7,47.7,1,D("Partly cloudy","//cdn.weatherapi.com/weather/64x64/day/116.png",1003),24.2,38.9,260,"W",1005,29.68,0,0,74,75,4.4,39.9,10,6,2,33.1,53.3))},{"query":A("any-internal-id","London",B("London","City of London, Greater London","United Kingdom",51.52,-0.11,"Europe/London",1673620218,"2023-01-13 14:30"),C(1673620200,"2023-01-13 14:30",11,51.8,1,D("Partly cloudy","//cdn.weatherapi.com/weather/64x64/day/116.png",1003),23,37.1,270,"W",1010,29.83,0,0,58,75,8.1,46.5,10,6,2,22.4,36))},{"query":A("us-zipcode-id-765","90201",B("Bell","California","USA",33.97,-118.17,"America/Los_Angeles",1673620220,"2023-01-13 6:30"),C(1673620200,"2023-01-13 06:30",10,50,0,D("Clear","//cdn.weatherapi.com/weather/64x64/night/113.png",1000),2.2,3.6,10,"N",1020,30.13,0,0,74,0,10.3,50.5,16,9,1,3.6,5.8))}]}`
)

func TestTronWeatherExample(t *testing.T) {
	if got := tronFromJSON(t, tronWeatherJSON); got != tronWeatherWant {
		t.Errorf("weather example mismatch:\ngot:\n%s\nwant:\n%s", got, tronWeatherWant)
	}
}
