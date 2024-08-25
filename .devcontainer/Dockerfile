FROM golang:latest

# Install necessary packages, Neovim, and Zsh
RUN apt-get update && apt-get install -y \
    cmake \
    ninja-build \
    gettext \
    git \
    unzip \
    curl \
    neovim \
    zsh \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Set Zsh as the default shell
SHELL ["/bin/zsh", "-c"]

# Set up the workspace
WORKDIR /workspace

# Set environment variables
ENV PATH="/go/bin:${PATH}"
ENV GOPATH="/go"
ENV SHELL="/bin/zsh"

# Verify Neovim installation
RUN nvim --version

# Optional: Install Oh My Zsh for a better Zsh experience
RUN sh -c "$(curl -fsSL https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/tools/install.sh)" "" --unattended

# Set Zsh as the default shell for root
RUN chsh -s /bin/zsh root
