FROM golang:1.27.2 AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILDDATE=unknown
ENV LDFLAGS="-s -w -X github.com/yuriy-kovalchuk/talos-monitoring/internal/version.Version=${VERSION} -X github.com/yuriy-kovalchuk/talos-monitoring/internal/version.Commit=${COMMIT} -X github.com/yuriy-kovalchuk/talos-monitoring/internal/version.BuildDate=${BUILDDATE}"
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="${LDFLAGS}" -o /out/talos-monitoring ./cmd/talos-monitoring

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/talos-monitoring /talos-monitoring
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/talos-monitoring"]
CMD ["serve"]
