{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-25.11-darwin";
  };

  outputs = { self, nixpkgs }:
    let
      system = "aarch64-darwin";
      pkgs = import nixpkgs { inherit system; };

      docker-mcp = pkgs.stdenvNoCC.mkDerivation rec {
        pname = "docker-mcp";
        version = "0.37.0";

        src = pkgs.fetchurl {
          url = "https://github.com/docker/mcp-gateway/releases/download/v${version}/docker-mcp-darwin-arm64.tar.gz";
          sha256 = "sha256:104a9da9c3d60017aa95f15fec370e40a2e8a6ec0d5e4db183d42127f16510d4";
        };

        unpackPhase = ''
          mkdir -p unpack
          tar -xzf "$src" -C unpack
        '';

        installPhase = ''
          mkdir -p $out/bin
          if [ -f unpack/docker-mcp ]; then
            cp unpack/docker-mcp $out/bin/docker-mcp
          else
            cp unpack/*/docker-mcp $out/bin/docker-mcp
          fi
          chmod +x $out/bin/docker-mcp
        '';
      };

    in
    {
      packages.${system} = {
        inherit docker-mcp;
      };

      devShells.${system}.default = pkgs.mkShell {
        packages = [
          pkgs.go
          pkgs.docker-client

          docker-mcp
        ];

        shellHook = ''
          export DOCKER_CONFIG="$PWD/.docker"
          mkdir -p "$DOCKER_CONFIG/cli-plugins"

          ln -sf "${docker-mcp}/bin/docker-mcp" \
            "$DOCKER_CONFIG/cli-plugins/docker-mcp"

          echo "h9s devshell ready"
          echo "  go $(go version | cut -d' ' -f3)"
          echo "  docker-mcp: try 'docker mcp --help'"
        '';
      };
    };
}
