# Builds either demo service: docker build --build-arg APP=connector|bridge .
FROM golang:1.26-alpine AS build
ARG APP
WORKDIR /src
COPY go.mod ./
COPY internal/ internal/
COPY ${APP}/ ${APP}/
RUN test -n "$APP" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./${APP}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app"]
