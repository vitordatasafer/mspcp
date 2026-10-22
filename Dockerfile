FROM node:22 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mspvdc ./cmd/mspvdc

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mspvdc /usr/local/bin/mspvdc
COPY --from=web /web/dist /srv/web
ENV WEB_DIR=/srv/web
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mspvdc"]
CMD ["api"]
