package stubnet_test

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/caprico99278/FledgeFlow-oss/go/stubnet"
)

// A dry run: the adapters keep their real HTTP code, but the only "internet"
// is the stub. Anything not registered is refused and recorded.
func Example() {
	net := stubnet.New()
	net.Handle("api.example.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	restore := net.InstallDefault() // http.DefaultClient now goes through the stub
	defer restore()

	resp, err := http.Post("https://api.example.test/posts?token=never-recorded", "application/json", nil)
	if err == nil {
		fmt.Println("posted:", resp.StatusCode)
		_ = resp.Body.Close()
	}
	_, err = http.Get("https://somewhere-else.example.test/")
	fmt.Println("blocked:", errors.Is(err, stubnet.ErrBlocked))

	for _, e := range net.Journal() {
		fmt.Println(e.Method, e.Host+e.Path, e.Status, e.Blocked)
	}
	// Output:
	// posted: 201
	// blocked: true
	// POST api.example.test/posts 201 false
	// GET somewhere-else.example.test/ 0 true
}
