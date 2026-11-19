{ pkgs, ... }:
{
  environment.systemPackages = with pkgs; [ git openssh postgresql_17 ];
  services.postgresql = {
    enable = true;
    package = pkgs.postgresql_17;
    enableTCPIP = false;
    ensureDatabases = [ "hk" ];
    ensureUsers = [ { name = "hk"; ensureDBOwnership = true; } ];
    authentication = ''
      local all all peer
    '';
  };
}
