# syntax=docker/dockerfile:1
FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /
COPY ./dist/go-galaxy /go-galaxy
ENTRYPOINT [ "/go-galaxy" ]
