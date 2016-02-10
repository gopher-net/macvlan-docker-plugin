FROM golang:1.5
RUN go get github.com/tools/godep
COPY . /go/src/github.com/gopher-net/macvlan-docker-plugin
WORKDIR /go/src/github.com/gopher-net/macvlan-docker-plugin
RUN godep go install -v
ENTRYPOINT ["macvlan-docker-plugin"]
