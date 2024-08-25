{
  packageOverrides = pkgs: with pkgs; {
    myPackages = pkgs.buildEnv {
      name = "dotfiles-dev";
      paths = [
        bash-completion
        neovim
        gnused
        cmake
        go
        nodejs_22
        starship
        fd
        ripgrep
        tenv
        zoxide
        fzf
        eza
        dive
      ];
    };
  };
}
