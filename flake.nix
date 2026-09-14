{
  description = "Loom dev environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      nixpkgs,
      flake-utils,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs {
          inherit system;
        };
      in
      {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            # Go services
            go_1_27
            gopls
            golangci-lint
            gotools
            delve

            # Go/protobuf codegen
            protoc-gen-go
            protoc-gen-connect-go
            protoc-gen-go-grpc

            # Helm chart validation (helm lint / helm template)
            kubernetes-helm

            # Proto codegen
            curl
            protobuf
            pkg-config
          ];

          # Corepack refuses to write shims into the read-only Nix store, so
          # point it at a project-local, gitignored directory instead.
          shellHook = "";
        };
      }
    );
}
