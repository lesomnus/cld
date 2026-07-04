variable "TAG" {
  default = "local"
}
variable "REPO" {
  default = "ghcr.io/lesomnus/cld"
}
variable "BUILD_HASH" {
  default = "0000000000000000000000000000000000000000"
}
variable "BUILD_TIMESTAMP" {
  default = "${timestamp()}"
}
variable "BUILD_DATE" {
  default = "${formatdate("YYMMDD", BUILD_TIMESTAMP)}"
}
variable "BUILD_ID" {
  default = "r0"
}
variable "APP_VERSION" {
  default = "${BUILD_DATE}-${BUILD_ID}"
}

target "build" {
  target = "build"
  args = {
    BUILD_HASH  = BUILD_HASH
    BUILD_ID    = BUILD_ID
    APP_VERSION = APP_VERSION
  }
  output = [{
    type = "local"
    dest = "dist"
  }]
}
target "runner" {
  target = "runner"
  labels = {
    "org.opencontainers.image.title"    = "cld-runner",
    "org.opencontainers.image.url"      = "https://github.com/lesomnus/cld",
    "org.opencontainers.image.revision" = "${BUILD_HASH}",
    "org.opencontainers.image.version"  = "${APP_VERSION}",
  }
  tags = [
    "${REPO}:runner",
    "${REPO}:runner-${BUILD_DATE}-${BUILD_ID}",
  ]
}
target "app" {
  target = "app"
  context = "./dist"
  dockerfile = "../Dockerfile"
  labels = {
    "org.opencontainers.image.title"         = "cld",
    # "org.opencontainers.image.description"   = "",
    # "org.opencontainers.image.documentation" = "",
    "org.opencontainers.image.url"           = "https://github.com/lesomnus/cld",
    # "org.opencontainers.image.vendor"        = "",
    "org.opencontainers.image.revision"      = "${BUILD_HASH}",
    "org.opencontainers.image.version"       = "${APP_VERSION}",
  }
  tags = [
    "${REPO}:${TAG}",
    "${REPO}:${BUILD_ID}",
    "${REPO}:${BUILD_DATE}",
    "${REPO}:${BUILD_DATE}-${BUILD_ID}",
  ]
}
