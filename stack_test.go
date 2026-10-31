package tracepanel

import "testing"

func TestAProjectFileIsNamedFromTheWorkingDirectoryInForwardSlashesOnEveryOS(t *testing.T) {
	cases := []struct {
		file, wd string
		windows  bool
		want     string
	}{
		{"/src/shop/books.go", "/src/shop", false, "books.go"},
		{"/src/shop/internal/books.go", "/src/shop/", false, "internal/books.go"},
		{"/usr/local/go/src/net/http/server.go", "/src/shop", false, "/usr/local/go/src/net/http/server.go"},
		{"/src/shopping/books.go", "/src/shop", false, "/src/shopping/books.go"},
		{"/src/Shop/books.go", "/src/shop", false, "/src/Shop/books.go"},
		{"/books.go", "/", false, "books.go"},
		// Go names a frame's file in forward slashes on Windows too; the working directory is in backslashes.
		{"D:/a/shop/books.go", `D:\a\shop`, true, "books.go"},
		{"D:/a/shop/internal/books.go", `d:\A\Shop`, true, "internal/books.go"},
		{`D:\a\shop\internal\books.go`, `D:\a\shop`, true, "internal/books.go"},
		{"C:/hostedtoolcache/windows/go/1.27.3/x64/src/net/http/server.go", `D:\a\shop`, true,
			"C:/hostedtoolcache/windows/go/1.27.3/x64/src/net/http/server.go"},
		{"D:/a/shop", `D:\a\shop`, true, "D:/a/shop"},
	}
	for _, c := range cases {
		if got := relativeTo(c.file, c.wd, c.windows); got != c.want {
			t.Errorf("relativeTo(%q, %q, %v) = %q, want %q", c.file, c.wd, c.windows, got, c.want)
		}
	}
}
