#pragma once

#include <cctype>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <mutex>
#include <vector>

#if __has_include("esphome/components/lilygo_t5_47_plus/display.h")
#include "esphome/components/lilygo_t5_47_plus/display.h"
#define BRD_BLIT_HAS_ESPHOME_DISPLAY 1
#endif

namespace brd_blit {

constexpr size_t PANEL_WIDTH = 960;
constexpr size_t PANEL_HEIGHT = 540;
constexpr size_t FRAMEBUFFER_SIZE = PANEL_WIDTH * PANEL_HEIGHT / 2;
constexpr size_t SHA256_HEX_LEN = 64;

enum class BlitResult {
  OK = 0,
  ERROR_NULL_POINTER = 1,
  ERROR_SIZE_MISMATCH = 2,
  ERROR_TARGET_OVERFLOW = 3,
};

// Copy raw packed 4bpp framebuffer data with bounds verification.
inline BlitResult copy_framebuffer(uint8_t *dst, size_t dst_capacity, const uint8_t *src,
                                   size_t src_len) {
  if (dst == nullptr || src == nullptr) {
    return BlitResult::ERROR_NULL_POINTER;
  }
  if (src_len != FRAMEBUFFER_SIZE) {
    return BlitResult::ERROR_SIZE_MISMATCH;
  }
  if (dst_capacity < FRAMEBUFFER_SIZE) {
    return BlitResult::ERROR_TARGET_OVERFLOW;
  }
  std::memcpy(dst, src, FRAMEBUFFER_SIZE);
  return BlitResult::OK;
}

// Parse, trim, and normalize SHA-256 hex digest string.
inline bool parse_and_normalize_sha256(const char *input, size_t input_len, char output[65]) {
  if (input == nullptr || output == nullptr || input_len == 0) {
    return false;
  }

  size_t start = 0;
  while (start < input_len && std::isspace(static_cast<unsigned char>(input[start]))) {
    ++start;
  }

  size_t end = input_len;
  while (end > start && std::isspace(static_cast<unsigned char>(input[end - 1]))) {
    --end;
  }

  const size_t trimmed_len = end - start;
  if (trimmed_len != SHA256_HEX_LEN) {
    return false;
  }

  for (size_t i = 0; i < SHA256_HEX_LEN; ++i) {
    const unsigned char c = static_cast<unsigned char>(input[start + i]);
    if (!std::isxdigit(c)) {
      return false;
    }
    output[i] = static_cast<char>(std::tolower(c));
  }
  output[SHA256_HEX_LEN] = '\0';
  return true;
}

// Thread-safe buffer handoff container for image transfer.
class BlitBuffer {
 public:
  BlitBuffer() : storage_(FRAMEBUFFER_SIZE, 0) {}

  bool store(const uint8_t *data, size_t len) {
    if (data == nullptr || len != FRAMEBUFFER_SIZE) {
      return false;
    }
    std::lock_guard<std::mutex> lock(this->mutex_);
    std::memcpy(this->storage_.data(), data, FRAMEBUFFER_SIZE);
    this->valid_ = true;
    return true;
  }

  bool retrieve(uint8_t *dst, size_t dst_capacity) const {
    if (dst == nullptr || dst_capacity < FRAMEBUFFER_SIZE) {
      return false;
    }
    std::lock_guard<std::mutex> lock(this->mutex_);
    if (!this->valid_) {
      return false;
    }
    std::memcpy(dst, this->storage_.data(), FRAMEBUFFER_SIZE);
    return true;
  }

  bool is_valid() const {
    std::lock_guard<std::mutex> lock(this->mutex_);
    return this->valid_;
  }

  void invalidate() {
    std::lock_guard<std::mutex> lock(this->mutex_);
    this->valid_ = false;
  }

 private:
  mutable std::mutex mutex_;
  std::vector<uint8_t> storage_;
  bool valid_{false};
};

#if defined(BRD_BLIT_HAS_ESPHOME_DISPLAY)
class DisplayAccess : public esphome::lilygo_t5_47_plus::LilygoT5_47PlusDisplay {
 public:
  static uint8_t *framebuffer(esphome::lilygo_t5_47_plus::LilygoT5_47PlusDisplay *disp) {
    if (disp == nullptr) {
      return nullptr;
    }
    uint8_t *esphome::display::DisplayBuffer::*mp = &DisplayAccess::buffer_;
    return disp->*mp;
  }

  static size_t framebuffer_size(esphome::lilygo_t5_47_plus::LilygoT5_47PlusDisplay *disp) {
    if (disp == nullptr) {
      return 0;
    }
    size_t (esphome::lilygo_t5_47_plus::LilygoT5_47PlusDisplay::*mp)() =
        &DisplayAccess::get_buffer_length_;
    return (disp->*mp)();
  }
};

inline uint8_t *framebuffer(esphome::lilygo_t5_47_plus::LilygoT5_47PlusDisplay *disp) {
  return DisplayAccess::framebuffer(disp);
}

inline size_t framebuffer_size(esphome::lilygo_t5_47_plus::LilygoT5_47PlusDisplay *disp) {
  return DisplayAccess::framebuffer_size(disp);
}
#endif

}  // namespace brd_blit
