{
  description = "Habikat's fixed NixOS Git/PostgreSQL experiment";
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/02c087f3fe673c9e317f206a627cb6b910336dbe";
    microvm.url = "github:microvm-nix/microvm.nix/d70265960646e506757f8364e8585c17a06050e7";
    microvm.inputs.nixpkgs.follows = "nixpkgs";
  };
  outputs = { nixpkgs, microvm, ... }:
    let
      system = "x86_64-linux";
      guest = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [ microvm.nixosModules.microvm ./base.nix ./recipe.nix ];
      };
      pkgs = nixpkgs.legacyPackages.${system};
      cfg = guest.config;
      boot = pkgs.writeText "boot.json" (builtins.toJSON {
        profile = "nixos-v1";
        boot_args = "console=ttyS0,115200 reboot=k panic=1 i8042.noaux i8042.nomux i8042.dumbkbd ${toString cfg.microvm.kernelParams}";
        memory_mib = cfg.microvm.mem;
        vcpus = cfg.microvm.vcpu;
        enable_pci = true;
        disks = [ "system" "data" "seed" ];
      });
    in {
      nixosConfigurations.habikat = guest;
      packages.${system}.default = pkgs.runCommand "habikat-nixos-export" {} ''
        mkdir -p $out
        cp ${cfg.microvm.kernel.dev}/vmlinux $out/vmlinux
        cp ${cfg.microvm.initrdPath} $out/initrd
        cp ${cfg.microvm.storeDisk} $out/system.erofs
        cp ${boot} $out/boot.json
        cp ${pkgs.writeClosure [ cfg.system.build.toplevel ]} $out/closure-paths
      '';
    };
}
