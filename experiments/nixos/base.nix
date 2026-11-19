{ config, lib, pkgs, ... }:
{
  system.stateVersion = "26.05";
  networking.hostName = "habikat";
  networking.useDHCP = false;
  networking.firewall.enable = false;
  nix.enable = false;
  microvm = {
    hypervisor = "firecracker";
    socket = "api.sock";
    vcpu = 1;
    mem = 512;
    interfaces = [];
    shares = [];
    storeOnDisk = true;
    storeDiskType = "erofs";
    vsock.cid = 3;
  };
  fileSystems."/" = lib.mkForce {
    device = "/dev/vdb";
    fsType = "ext4";
    neededForBoot = true;
  };
  fileSystems."/run/provision/seed" = {
    device = "/dev/vdc";
    fsType = "ext4";
    options = [ "ro" "nosuid" "nodev" "noexec" ];
  };
  boot.kernelParams = [ "quiet" ];
  boot.kernelModules = [ "atkbd" ];
  boot.initrd.systemd.enable = true;
  users.mutableUsers = false;
  users.allowNoPasswordLogin = true;
  users.users.hk = {
    isNormalUser = true;
    uid = 1000;
    home = "/home/hk";
    createHome = true;
    hashedPassword = "!";
  };
  users.users.root.hashedPassword = "!";
  security.sudo.enable = false;
  services.openssh = {
    enable = true;
    openFirewall = false;
    hostKeys = [];
    generateHostKeys = false;
    authorizedKeysFiles = [ "/run/habikat-ssh/authorized_keys" ];
    settings = {
      PermitRootLogin = "no";
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
      AllowUsers = [ "hk" ];
      AllowAgentForwarding = false;
      AllowTcpForwarding = false;
      X11Forwarding = false;
      HostKey = "/run/habikat-ssh/host_key";
    };
  };
  systemd.services.sshd.enable = false;
  systemd.services."sshd@".enable = false;
  systemd.services."sshd-vsock@".enable = false;
  systemd.sockets.sshd-vsock.enable = false;
  systemd.sockets.sshd.enable = false;
  systemd.generators.systemd-ssh-generator = "/dev/null";
  systemd.sockets.habikat-ssh = {
    wantedBy = [ "sockets.target" ];
    requires = [ "habikat-credentials.service" ];
    after = [ "habikat-credentials.service" ];
    socketConfig = {
      ListenStream = "vsock::22";
      Accept = true;
    };
  };
  systemd.services."habikat-ssh@" = {
    requires = [ "habikat-credentials.service" ];
    after = [ "habikat-credentials.service" ];
    serviceConfig = {
      ExecStart = "${pkgs.openssh}/bin/sshd -i -e -f /etc/ssh/sshd_config";
      StandardInput = "socket";
      StandardError = "journal";
    };
  };
  systemd.services.habikat-credentials = {
    wantedBy = [ "multi-user.target" ];
    requires = [ "run-provision-seed.mount" ];
    after = [ "run-provision-seed.mount" ];
    before = [ "habikat-ssh.socket" "shutdown.target" ];
    conflicts = [ "shutdown.target" ];
    unitConfig.DefaultDependencies = false;
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      UMask = "0077";
    };
    script = ''
      chmod 0700 /run/provision
      install -d -m 0755 /run/habikat-ssh
      install -m 0600 /run/provision/seed/host_key /run/habikat-ssh/host_key
      install -m 0644 /run/provision/seed/authorized_keys /run/habikat-ssh/authorized_keys
      ${pkgs.openssh}/bin/sshd -t -f /etc/ssh/sshd_config
    '';
  };
  systemd.ctrlAltDelUnit = "reboot.target";
  services.journald.settings.Journal = {
    Storage = "persistent";
    ForwardToConsole = true;
  };
  environment.etc.bash_logout.text = lib.mkForce "";
  environment.sessionVariables.HISTFILE = "/home/hk/.ash_history";
  systemd.tmpfiles.rules = [
    "L+ /bin/busybox - - - - ${pkgs.busybox}/bin/busybox"
    "L+ /usr/bin/git - - - - ${pkgs.git}/bin/git"
    "L+ /usr/bin/grep - - - - ${pkgs.gnugrep}/bin/grep"
    "L+ /usr/bin/cat - - - - ${pkgs.coreutils}/bin/cat"
    "L+ /usr/bin/rm - - - - ${pkgs.coreutils}/bin/rm"
  ];
  environment.systemPackages = with pkgs; [ coreutils util-linux procps ];
}
