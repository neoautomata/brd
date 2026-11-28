#pragma once

#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <mutex>
#include <vector>

#if defined(ESP_PLATFORM) && __has_include(<esp_heap_caps.h>)
#include <esp_heap_caps.h>
#endif

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

// Return true when Content-Length is unknown (0 or SIZE_MAX) or matches expected size.
constexpr bool is_expected_content_length(size_t content_length, size_t expected) {
  return content_length == 0U || content_length == static_cast<size_t>(-1) ||
         content_length == expected;
}

// Allocate a staging buffer from PSRAM on ESP32 when available.
class StagingBuffer {
 public:
  explicit StagingBuffer(size_t size) : size_(size) {
    if (size == 0U) {
      return;
    }
#if defined(ESP_PLATFORM) && __has_include(<esp_heap_caps.h>)
    this->data_ =
        static_cast<uint8_t *>(heap_caps_malloc(size, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (this->data_ != nullptr) {
      this->from_heap_caps_ = true;
    } else {
      this->data_ = static_cast<uint8_t *>(std::malloc(size));
    }
#else
    this->data_ = static_cast<uint8_t *>(std::malloc(size));
#endif
    if (this->data_ == nullptr) {
      this->size_ = 0U;
    }
  }

  ~StagingBuffer() {
    if (this->data_ != nullptr) {
#if defined(ESP_PLATFORM) && __has_include(<esp_heap_caps.h>)
      if (this->from_heap_caps_) {
        heap_caps_free(this->data_);
        return;
      }
#endif
      std::free(this->data_);
    }
  }

  StagingBuffer(const StagingBuffer &) = delete;
  StagingBuffer &operator=(const StagingBuffer &) = delete;

  uint8_t *data() { return this->data_; }
  const uint8_t *data() const { return this->data_; }
  size_t size() const { return this->size_; }
  bool valid() const { return this->data_ != nullptr; }

 private:
  uint8_t *data_{nullptr};
  size_t size_{0};
#if defined(ESP_PLATFORM) && __has_include(<esp_heap_caps.h>)
  bool from_heap_caps_{false};
#endif
};

// Copy a packed 4bpp framebuffer and check its bounds.
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
  std::memmove(dst, src, FRAMEBUFFER_SIZE);
  return BlitResult::OK;
}

namespace detail {

constexpr bool is_ascii_space(unsigned char c) {
  return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f';
}

constexpr int ascii_hex_value(unsigned char c) {
  if (c >= '0' && c <= '9') {
    return static_cast<int>(c - '0');
  }
  if (c >= 'a' && c <= 'f') {
    return static_cast<int>(c - 'a' + 10);
  }
  if (c >= 'A' && c <= 'F') {
    return static_cast<int>(c - 'A' + 10);
  }
  return -1;
}

constexpr char ascii_hex_lower(unsigned char c) {
  return (c >= 'A' && c <= 'F') ? static_cast<char>(c - 'A' + 'a') : static_cast<char>(c);
}

}  // namespace detail

// Trim whitespace and convert a SHA-256 hex digest to lowercase.
// The firmware uses /img.sha only to detect image changes, not as a security check.
// The firmware does not calculate SHA-256 on the device to save CPU time and power.
inline bool parse_and_normalize_sha256(const char *input, size_t input_len, char output[65]) {
  if (output != nullptr) {
    output[0] = '\0';
  }
  if (input == nullptr || output == nullptr || input_len == 0U) {
    return false;
  }

  size_t start = 0U;
  while (start < input_len && detail::is_ascii_space(static_cast<unsigned char>(input[start]))) {
    ++start;
  }

  size_t end = input_len;
  while (end > start && detail::is_ascii_space(static_cast<unsigned char>(input[end - 1U]))) {
    --end;
  }

  const size_t trimmed_len = end - start;
  if (trimmed_len != SHA256_HEX_LEN) {
    return false;
  }

  for (size_t i = 0U; i < SHA256_HEX_LEN; ++i) {
    const unsigned char c = static_cast<unsigned char>(input[start + i]);
    if (detail::ascii_hex_value(c) < 0) {
      output[0] = '\0';
      return false;
    }
    output[i] = detail::ascii_hex_lower(c);
  }
  output[SHA256_HEX_LEN] = '\0';
  return true;
}

// Store and retrieve a framebuffer safely across threads.
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
