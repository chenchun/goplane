# goplane (part of Ryu SDN Framework)
#

FROM osrg/gobgp

ENV GO15VENDOREXPERIMENT 1
ADD . $GOPATH/src/github.com/osrg/goplane/
RUN cd $GOPATH/src/github.com/osrg/goplane
RUN go install github.com/osrg/goplane
RUN go get github.com/socketplane/libovsdb
RUN cd $GOPATH/src/github.com/osrg/goplane/vendor/github.com/osrg/gobgp/gobgpd && go install
RUN cd $GOPATH/src/github.com/osrg/goplane/vendor/github.com/osrg/gobgp/gobgp && go install
