# Generated from packaging/homebrew/quantifai-sync.rb in nino-chavez/quantifai-sync
# by `make formula`. Edit the template there; the tap copy is replaced on release.
class QuantifaiSync < Formula
  desc "Telemetry sync agent for Quantifai — streams AI tool usage to your dashboard"
  homepage "https://quantifai.app"
  version "VERSION"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/nino-chavez/quantifai-sync/releases/download/vVERSION/quantifai-sync-darwin-arm64.tar.gz"
      sha256 "SHA256_DARWIN_ARM64"
    else
      url "https://github.com/nino-chavez/quantifai-sync/releases/download/vVERSION/quantifai-sync-darwin-amd64.tar.gz"
      sha256 "SHA256_DARWIN_AMD64"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/nino-chavez/quantifai-sync/releases/download/vVERSION/quantifai-sync-linux-arm64.tar.gz"
      sha256 "SHA256_LINUX_ARM64"
    else
      url "https://github.com/nino-chavez/quantifai-sync/releases/download/vVERSION/quantifai-sync-linux-amd64.tar.gz"
      sha256 "SHA256_LINUX_AMD64"
    end
  end

  def install
    # Each release archive holds one binary named for its platform,
    # e.g. quantifai-sync-darwin-arm64.
    Dir.glob("quantifai-sync-*").each do |f|
      next if f.end_with?(".sha256")

      bin.install f => "quantifai-sync"
    end
  end

  service do
    run [opt_bin/"quantifai-sync", "run"]
    keep_alive true
    log_path var/"log/quantifai-sync.log"
    error_log_path var/"log/quantifai-sync.log"
  end

  def caveats
    <<~EOS
      Store your API key and register the background service:
        quantifai-sync install --api-key YOUR_KEY
    EOS
  end

  test do
    assert_match "quantifai-sync", shell_output("#{bin}/quantifai-sync version")
  end
end
