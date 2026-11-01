package rank

import (
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestBM25TermKeysOwnStorage(t *testing.T) {
	sources := []string{
		strings.Repeat(" ", 1<<20) + "shared_term",
		strings.Repeat("\t", 1<<20) + "shared_term",
	}
	bm := NewBM25()
	assertIndependentStorage := func() {
		t.Helper()
		value := reflect.ValueOf(bm).Elem()
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if field.Kind() != reflect.Map || field.Type().Key().Kind() != reflect.String {
				continue
			}
			for _, key := range field.MapKeys() {
				address := key.Pointer()
				for _, source := range sources {
					start := reflect.ValueOf(source).Pointer()
					if address >= start && address-start < uintptr(len(source)) {
						t.Fatal("term key retains source backing storage")
					}
				}
			}
		}
	}
	for i, source := range sources {
		bm.AddDocument(strconv.Itoa(i), Tokenize(source))
		assertIndependentStorage()
	}
	runtime.KeepAlive(sources)
	runtime.KeepAlive(bm)
}
