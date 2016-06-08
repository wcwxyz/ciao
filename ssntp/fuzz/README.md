# Requirements

```
$ go get github.com/dvyukov/go-fuzz/go-fuzz
$ go get github.com/dvyukov/go-fuzz/go-fuzz-build
```

# Client fuzzing

```
$ go-fuzz-build github.com/01org/ciao/ssntp/fuzz/client
$ go-fuzz -bin ssntpclient-fuzz.zip -workdir $GOPATH/src/github.com/01org/ciao/ssntp/fuzz/
```