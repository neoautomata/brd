#include <cassert>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <limits>
#include <thread>
#include <vector>

#include "../brd_blit.h"
#include "../brd_schedule.h"
#include "../brd_rtc.h"
#include "../brd_power.h"

// Mock I2C implementation for testing retries and register protocol.
class MockI2cBus : public brd_rtc::I2cPort {
 public:
  struct Operation {
    uint8_t address;
    std::vector<uint8_t> write_data;
    bool is_read;
  };

  std::vector<Operation> history;
  uint8_t registers[256]{};
  int fail_count{0};
  bool inject_readback_mismatch{false};

  MockI2cBus() {
    std::memset(this->registers, 0, sizeof(this->registers));
  }

  bool write(uint8_t address, const uint8_t *data, size_t len) override {
    if (this->fail_count > 0) {
      this->fail_count--;
      return false;
    }
    this->history.push_back({address, std::vector<uint8_t>(data, data + len), false});
    if (len >= 1) {
      uint8_t reg = data[0];
      for (size_t i = 1; i < len; ++i) {
        this->registers[reg + (i - 1)] = data[i];
      }
    }
    return true;
  }

  bool write_read(uint8_t address, const uint8_t *write_data, size_t write_len,
                  uint8_t *read_data, size_t read_len) override {
    if (this->fail_count > 0) {
      this->fail_count--;
      return false;
    }
    this->history.push_back({address, std::vector<uint8_t>(write_data, write_data + write_len), true});
    if (write_len >= 1 && read_len > 0) {
      uint8_t reg = write_data[0];
      for (size_t i = 0; i < read_len; ++i) {
        if (this->inject_readback_mismatch) {
          read_data[i] = 0xFF;
        } else {
          read_data[i] = this->registers[reg + i];
        }
      }
    }
    return true;
  }
};

static void test_blit_bounds_and_errors() {
  printf("[TEST] brd_blit framebuffer copy and bounds verification...\n");

  std::vector<uint8_t> dst(brd_blit::FRAMEBUFFER_SIZE);
  std::vector<uint8_t> src(brd_blit::FRAMEBUFFER_SIZE, 0xAB);

  // Exact size copy succeeds.
  auto res = brd_blit::copy_framebuffer(dst.data(), dst.size(), src.data(), src.size());
  assert(res == brd_blit::BlitResult::OK);
  assert(dst[0] == 0xAB && dst[brd_blit::FRAMEBUFFER_SIZE - 1] == 0xAB);

  // Null pointer checks return errors.
  assert(brd_blit::copy_framebuffer(nullptr, dst.size(), src.data(), src.size()) ==
         brd_blit::BlitResult::ERROR_NULL_POINTER);
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size(), nullptr, src.size()) ==
         brd_blit::BlitResult::ERROR_NULL_POINTER);

  // Truncated source returns error.
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size(), src.data(), src.size() - 1) ==
         brd_blit::BlitResult::ERROR_SIZE_MISMATCH);

  // Oversized source returns error.
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size(), src.data(), src.size() + 1) ==
         brd_blit::BlitResult::ERROR_SIZE_MISMATCH);

  // Target overflow returns error.
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size() - 1, src.data(), src.size()) ==
         brd_blit::BlitResult::ERROR_TARGET_OVERFLOW);

  // Content-Length pre-check allows unknown (0 or SIZE_MAX) and exact match, rejects mismatches.
  assert(brd_blit::is_expected_content_length(0U, brd_blit::FRAMEBUFFER_SIZE));
  assert(brd_blit::is_expected_content_length(static_cast<size_t>(-1), brd_blit::FRAMEBUFFER_SIZE));
  assert(brd_blit::is_expected_content_length(brd_blit::FRAMEBUFFER_SIZE, brd_blit::FRAMEBUFFER_SIZE));
  assert(!brd_blit::is_expected_content_length(1U, brd_blit::FRAMEBUFFER_SIZE));
  assert(!brd_blit::is_expected_content_length(brd_blit::FRAMEBUFFER_SIZE - 1, brd_blit::FRAMEBUFFER_SIZE));
  assert(!brd_blit::is_expected_content_length(brd_blit::FRAMEBUFFER_SIZE + 1, brd_blit::FRAMEBUFFER_SIZE));

  // StagingBuffer RAII allocation and copy into framebuffer.
  brd_blit::StagingBuffer empty_stage(0U);
  assert(!empty_stage.valid() && empty_stage.data() == nullptr && empty_stage.size() == 0U);
  brd_blit::StagingBuffer stage(brd_blit::FRAMEBUFFER_SIZE);
  assert(stage.valid() && stage.data() != nullptr && stage.size() == brd_blit::FRAMEBUFFER_SIZE);
  std::memset(stage.data(), 0x5A, stage.size());
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size(), stage.data(), stage.size()) ==
         brd_blit::BlitResult::OK);
  assert(dst[0] == 0x5A && dst[brd_blit::FRAMEBUFFER_SIZE - 1] == 0x5A);

  printf("  -> Passed blit bounds checks.\n");
}

static void test_blit_sha256_normalizer() {
  printf("[TEST] brd_blit SHA-256 parser and normalizer...\n");

  char out[65];

  // Standard valid lowercase hex.
  const char *valid_lower = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
  assert(brd_blit::parse_and_normalize_sha256(valid_lower, strlen(valid_lower), out));
  assert(std::strcmp(out, valid_lower) == 0);

  // Mixed case with whitespace and newline.
  const char *mixed_ws = "  E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855 \r\n";
  assert(brd_blit::parse_and_normalize_sha256(mixed_ws, strlen(mixed_ws), out));
  assert(std::strcmp(out, valid_lower) == 0);

  // Null and empty checks fail.
  assert(!brd_blit::parse_and_normalize_sha256(nullptr, 64, out));
  assert(!brd_blit::parse_and_normalize_sha256(valid_lower, 0, out));
  assert(!brd_blit::parse_and_normalize_sha256(valid_lower, 64, nullptr));

  // Too short input fails.
  const char *short_sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85";
  assert(!brd_blit::parse_and_normalize_sha256(short_sha, strlen(short_sha), out));

  // Too long input fails.
  const char *long_sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855aa";
  assert(!brd_blit::parse_and_normalize_sha256(long_sha, strlen(long_sha), out));

  // Invalid non-hex characters fail.
  const char *invalid_hex = "g3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
  assert(!brd_blit::parse_and_normalize_sha256(invalid_hex, strlen(invalid_hex), out));

  printf("  -> Passed SHA-256 normalizer checks.\n");
}

static void test_blit_thread_safe_buffer() {
  printf("[TEST] brd_blit BlitBuffer thread safety...\n");

  brd_blit::BlitBuffer buffer;
  assert(!buffer.is_valid());

  std::vector<uint8_t> data(brd_blit::FRAMEBUFFER_SIZE, 0x42);

  assert(buffer.store(data.data(), data.size()));
  assert(buffer.is_valid());

  std::vector<uint8_t> read_back(brd_blit::FRAMEBUFFER_SIZE, 0x00);
  assert(buffer.retrieve(read_back.data(), read_back.size()));
  assert(read_back[0] == 0x42 && read_back[brd_blit::FRAMEBUFFER_SIZE - 1] == 0x42);

  buffer.invalidate();
  assert(!buffer.is_valid());
  assert(!buffer.retrieve(read_back.data(), read_back.size()));

  printf("  -> Passed BlitBuffer basic operations.\n");
}

static void test_schedule_civil_normalization() {
  printf("[TEST] brd_schedule days_in_month and normalize_civil_datetime...\n");

  assert(brd_schedule::days_in_month(2025, 0) == 0);
  assert(brd_schedule::days_in_month(2025, 13) == 0);
  assert(brd_schedule::days_in_month(2024, 2) == 29);
  assert(brd_schedule::days_in_month(2025, 2) == 28);
  assert(brd_schedule::days_in_month(2000, 2) == 29);
  assert(brd_schedule::days_in_month(2100, 2) == 28);
  assert(brd_schedule::days_in_month(2026, 3) == 31);
  assert(brd_schedule::days_in_month(2026, 4) == 30);

  struct NormCase {
    brd_schedule::LocalTime in;
    brd_schedule::LocalTime out;
  };
  const NormCase cases[] = {
      // Non-leap Feb 28 -> Mar 1 on hour overflow.
      {{2025, 2, 28, 24, 0, 0}, {2025, 3, 1, 0, 0, 0}},
      // Leap Feb 28 -> Feb 29, and Feb 29 -> Mar 1.
      {{2024, 2, 28, 24, 0, 0}, {2024, 2, 29, 0, 0, 0}},
      {{2024, 2, 29, 25, 0, 0}, {2024, 3, 1, 1, 0, 0}},
      // Century non-leap and 400-year leap boundaries.
      {{2100, 2, 28, 24, 0, 0}, {2100, 3, 1, 0, 0, 0}},
      {{2000, 2, 28, 24, 0, 0}, {2000, 2, 29, 0, 0, 0}},
      // Mar 31 -> Apr 1 and Dec 31 -> Jan 1 carry.
      {{2026, 3, 31, 24, 0, 0}, {2026, 4, 1, 0, 0, 0}},
      {{2026, 12, 31, 23, 60, 0}, {2027, 1, 1, 0, 0, 0}},
      {{2026, 12, 31, 23, 59, 60}, {2027, 1, 1, 0, 0, 0}},
      // Multi-unit overflow across seconds, minutes, hours, and months.
      {{2026, 1, 31, 50, 125, -120}, {2026, 2, 2, 4, 3, 0}},
      {{2026, 13, 1, 0, 0, 0}, {2027, 1, 1, 0, 0, 0}},
      // Borrow across midnight, months, and leap years.
      {{2026, 1, 1, 0, -1, 0}, {2025, 12, 31, 23, 59, 0}},
      {{2026, 1, 1, -25, 0, 0}, {2025, 12, 30, 23, 0, 0}},
      {{2024, 3, 1, 0, 0, -1}, {2024, 2, 29, 23, 59, 59}},
      {{2025, 3, 1, 0, 0, -1}, {2025, 2, 28, 23, 59, 59}},
      {{2026, 0, 15, 0, 0, 0}, {2025, 12, 15, 0, 0, 0}},
      {{2026, 3, 0, 12, 0, 0}, {2026, 2, 28, 12, 0, 0}},
  };

  for (const auto &c : cases) {
    brd_schedule::LocalTime actual = c.in;
    assert(brd_schedule::normalize_civil_datetime(actual));
    assert(actual.year == c.out.year);
    assert(actual.month == c.out.month);
    assert(actual.day == c.out.day);
    assert(actual.hour == c.out.hour);
    assert(actual.minute == c.out.minute);
    assert(actual.second == c.out.second);
  }

  brd_schedule::LocalTime bad{0, 1, 1, 0, 0, 0};
  assert(!brd_schedule::normalize_civil_datetime(bad));

  printf("  -> Passed civil datetime normalization checks.\n");
}

static void test_schedule_dst_and_boundaries() {
  printf("[TEST] brd_schedule POSIX CET/CEST and UTC transitions...\n");

  brd_schedule::PosixTzCalendar cal("CET-1CEST,M3.5.0,M10.5.0/3");
  brd_schedule::PosixTzCalendar cal_utc;
  brd_schedule::ScheduleConfig cfg;
  cfg.wake_hour = 6;
  cfg.wake_minute = 20;
  cfg.sleep_hour = 21;
  cfg.sleep_minute = 45;

  // Invalid clock returns fallback plan.
  auto plan_zero = brd_schedule::plan_next_wake(cal, 0, cfg);
  assert(!plan_zero.clock_valid);
  assert(std::strcmp(plan_zero.mode, "timer") == 0);
  assert(plan_zero.sleep_seconds == 900);

  // Pre-2020 epoch returns fallback plan.
  auto plan_old = brd_schedule::plan_next_wake(cal, 1000000, cfg);
  assert(!plan_old.clock_valid);

  // Test default PosixTzCalendar uses UTC.
  int64_t utc_noon = 0;
  assert(cal_utc.local_to_epoch({2026, 7, 15, 12, 0, 0}, utc_noon));
  brd_schedule::WallTime utc_check;
  assert(cal_utc.epoch_to_utc(utc_noon, utc_check));
  assert(utc_check.hour == 12 && utc_check.minute == 0 && utc_check.second == 0);

  // Test Spring-Forward DST Transition: March 29, 2026.
  brd_schedule::WallTime t_spring;
  t_spring.year = 2026;
  t_spring.month = 3;
  t_spring.day = 29;
  t_spring.hour = 10;
  t_spring.minute = 13;
  t_spring.second = 5;
  int64_t epoch_spring = 0;
  assert(cal.local_to_epoch(t_spring, epoch_spring));

  auto plan_spring = brd_schedule::plan_next_wake(cal, epoch_spring, cfg);
  assert(plan_spring.clock_valid);
  assert(std::strcmp(plan_spring.mode, "timer") == 0);
  assert(plan_spring.arm_alarm);

  brd_schedule::WallTime local_target;
  assert(cal.epoch_to_local(plan_spring.target_epoch, local_target));
  assert(local_target.hour == 10 && local_target.minute == 30 && local_target.second == 0);

  // Test Fall-Back DST Transition: October 25, 2026.
  brd_schedule::WallTime t_fall;
  t_fall.year = 2026;
  t_fall.month = 10;
  t_fall.day = 25;
  t_fall.hour = 14;
  t_fall.minute = 0;
  t_fall.second = 0;
  int64_t epoch_fall = 0;
  assert(cal.local_to_epoch(t_fall, epoch_fall));

  auto plan_fall = brd_schedule::plan_next_wake(cal, epoch_fall, cfg);
  assert(plan_fall.clock_valid);
  assert(cal.epoch_to_local(plan_fall.target_epoch, local_target));
  assert(local_target.hour == 14 && local_target.minute == 15 && local_target.second == 0);

  // Test 5-minute boundary interval: 14:00:10 -> 14:05:00, and 14:03:10 skips to 14:10:00 due to 120s lead margin.
  brd_schedule::ScheduleConfig cfg_5m = cfg;
  cfg_5m.boundary_interval_min = 5;
  cfg_5m.fallback_sleep_sec = 300;
  int64_t epoch_5m_a = 0;
  assert(cal.local_to_epoch({2026, 7, 15, 14, 0, 10}, epoch_5m_a));
  auto plan_5m_a = brd_schedule::plan_next_wake(cal, epoch_5m_a, cfg_5m);
  assert(plan_5m_a.clock_valid);
  assert(plan_5m_a.arm_alarm);
  assert(cal.epoch_to_local(plan_5m_a.target_epoch, local_target));
  assert(local_target.hour == 14 && local_target.minute == 5 && local_target.second == 0);

  int64_t epoch_5m_b = 0;
  assert(cal.local_to_epoch({2026, 7, 15, 14, 3, 10}, epoch_5m_b));
  auto plan_5m_b = brd_schedule::plan_next_wake(cal, epoch_5m_b, cfg_5m);
  assert(plan_5m_b.clock_valid);
  assert(plan_5m_b.arm_alarm);
  assert(cal.epoch_to_local(plan_5m_b.target_epoch, local_target));
  assert(local_target.hour == 14 && local_target.minute == 10 && local_target.second == 0);

  // Non-positive boundary_interval_min falls back to 15-minute boundaries instead of dividing by zero.
  brd_schedule::ScheduleConfig cfg_zero_int = cfg;
  cfg_zero_int.boundary_interval_min = 0;
  auto plan_zero_int = brd_schedule::plan_next_wake(cal, epoch_fall, cfg_zero_int);
  assert(plan_zero_int.clock_valid);
  assert(cal.epoch_to_local(plan_zero_int.target_epoch, local_target));
  assert(local_target.hour == 14 && local_target.minute == 15 && local_target.second == 0);

  // Test Bedtime Clamping: 21:43:00 has mode "timer".
  brd_schedule::WallTime t_bed_clamp;
  t_bed_clamp.year = 2026;
  t_bed_clamp.month = 7;
  t_bed_clamp.day = 15;
  t_bed_clamp.hour = 21;
  t_bed_clamp.minute = 43;
  t_bed_clamp.second = 0;
  int64_t epoch_bed_clamp = 0;
  assert(cal.local_to_epoch(t_bed_clamp, epoch_bed_clamp));

  auto plan_bed_clamp = brd_schedule::plan_next_wake(cal, epoch_bed_clamp, cfg);
  assert(plan_bed_clamp.clock_valid);
  assert(std::strcmp(plan_bed_clamp.mode, "timer") == 0);
  assert(cal.epoch_to_local(plan_bed_clamp.target_epoch, local_target));
  assert(local_target.hour == 21 && local_target.minute == 45 && local_target.second == 0);

  // Test Bedtime Advance: 21:44:10 advances to morning 06:20 with mode "alarm".
  brd_schedule::WallTime t_bed_adv;
  t_bed_adv.year = 2026;
  t_bed_adv.month = 7;
  t_bed_adv.day = 15;
  t_bed_adv.hour = 21;
  t_bed_adv.minute = 44;
  t_bed_adv.second = 10;
  int64_t epoch_bed_adv = 0;
  assert(cal.local_to_epoch(t_bed_adv, epoch_bed_adv));

  auto plan_bed_adv = brd_schedule::plan_next_wake(cal, epoch_bed_adv, cfg);
  assert(plan_bed_adv.clock_valid);
  assert(std::strcmp(plan_bed_adv.mode, "alarm") == 0);
  assert(cal.epoch_to_local(plan_bed_adv.target_epoch, local_target));
  assert(local_target.hour == 6 && local_target.minute == 20 && local_target.second == 0);
  assert(local_target.day == 16);

  // Test Overnight Sleep: 23:00:00 wakes at 06:20 next morning with mode "alarm".
  brd_schedule::WallTime t_night;
  t_night.year = 2026;
  t_night.month = 7;
  t_night.day = 15;
  t_night.hour = 23;
  t_night.minute = 0;
  t_night.second = 0;
  int64_t epoch_night = 0;
  assert(cal.local_to_epoch(t_night, epoch_night));

  auto plan_night = brd_schedule::plan_next_wake(cal, epoch_night, cfg);
  assert(plan_night.clock_valid);
  assert(std::strcmp(plan_night.mode, "alarm") == 0);
  assert(cal.epoch_to_local(plan_night.target_epoch, local_target));
  assert(local_target.hour == 6 && local_target.minute == 20 && local_target.second == 0);
  assert(local_target.day == 16);

  // Test Early Morning before Wake: 04:30:00 wakes at 06:20 today with mode "alarm".
  brd_schedule::WallTime t_early;
  t_early.year = 2026;
  t_early.month = 7;
  t_early.day = 16;
  t_early.hour = 4;
  t_early.minute = 30;
  t_early.second = 0;
  int64_t epoch_early = 0;
  assert(cal.local_to_epoch(t_early, epoch_early));

  auto plan_early = brd_schedule::plan_next_wake(cal, epoch_early, cfg);
  assert(plan_early.clock_valid);
  assert(std::strcmp(plan_early.mode, "alarm") == 0);
  assert(cal.epoch_to_local(plan_early.target_epoch, local_target));
  assert(local_target.hour == 6 && local_target.minute == 20 && local_target.second == 0);
  assert(local_target.day == 16);

  // Test Revalidation: remaining time >= 90s updates sleep_seconds and recalculates UTC target.
  brd_schedule::SchedulePlan input_plan = plan_spring;
  input_plan.target_utc_hour = 0;
  input_plan.target_utc_minute = 0;
  auto plan_reval_ok = brd_schedule::revalidate_before_sleep(cal, epoch_spring + 30, input_plan, cfg);
  assert(plan_reval_ok.target_epoch == plan_spring.target_epoch);
  assert(plan_reval_ok.sleep_seconds == plan_spring.sleep_seconds - 30);
  assert(plan_reval_ok.arm_alarm);
  assert(plan_reval_ok.target_utc_hour == plan_spring.target_utc_hour);
  assert(plan_reval_ok.target_utc_minute == plan_spring.target_utc_minute);

  // Test Revalidation: upgrades plan when target_epoch == 0.
  brd_schedule::SchedulePlan zero_plan;
  zero_plan.clock_valid = false;
  zero_plan.target_epoch = 0;
  auto plan_reval_upgraded = brd_schedule::revalidate_before_sleep(cal, epoch_spring, zero_plan, cfg);
  assert(plan_reval_upgraded.clock_valid);
  assert(plan_reval_upgraded.target_epoch == plan_spring.target_epoch);

  // Test Revalidation: remaining time < 90s recomputes schedule.
  auto plan_reval_stale = brd_schedule::revalidate_before_sleep(cal, plan_spring.target_epoch - 30, plan_spring, cfg);
  assert(plan_reval_stale.target_epoch != plan_spring.target_epoch);

  // Verify default ScheduleConfig (07:00..22:00) in UTC across daytime and overnight.
  brd_schedule::ScheduleConfig default_cfg;
  int64_t utc_day = 0;
  assert(cal_utc.local_to_epoch({2026, 7, 15, 12, 10, 0}, utc_day));
  const auto plan_utc_day = brd_schedule::plan_next_wake(cal_utc, utc_day, default_cfg);
  assert(plan_utc_day.clock_valid && plan_utc_day.arm_alarm);
  assert(plan_utc_day.target_utc_hour == 12 && plan_utc_day.target_utc_minute == 15);

  int64_t utc_night = 0;
  assert(cal_utc.local_to_epoch({2026, 7, 15, 22, 0, 0}, utc_night));
  const auto plan_utc_night = brd_schedule::plan_next_wake(cal_utc, utc_night, default_cfg);
  assert(plan_utc_night.clock_valid && plan_utc_night.arm_alarm);
  assert(std::strcmp(plan_utc_night.mode, "alarm") == 0);
  assert(plan_utc_night.target_utc_hour == 7 && plan_utc_night.target_utc_minute == 0);

  // Verify 21:45 -> 06:20 overnight UTC wake hour/minute explicitly in CET (05:20 UTC) and CEST (04:20 UTC).
  int64_t cet_night = 0;
  assert(cal.local_to_epoch({2026, 1, 15, 21, 45, 0}, cet_night));
  const auto plan_cet_night = brd_schedule::plan_next_wake(cal, cet_night, cfg);
  assert(plan_cet_night.arm_alarm);
  assert(plan_cet_night.target_utc_hour == 5 && plan_cet_night.target_utc_minute == 20);

  int64_t cest_night = 0;
  assert(cal.local_to_epoch({2026, 7, 15, 21, 45, 0}, cest_night));
  const auto plan_cest_night = brd_schedule::plan_next_wake(cal, cest_night, cfg);
  assert(plan_cest_night.arm_alarm);
  assert(plan_cest_night.target_utc_hour == 4 && plan_cest_night.target_utc_minute == 20);

  printf("  -> Passed DST and schedule boundary checks.\n");
}

static void test_rtc_bcd_and_retries() {
  printf("[TEST] brd_rtc BCD codec and I2C operations...\n");

  // BCD conversion table test across all 0..99 values.
  for (uint8_t i = 0; i < 100; ++i) {
    uint8_t bcd = brd_rtc::dec_to_bcd(i);
    uint8_t dec = brd_rtc::bcd_to_dec(bcd);
    assert(dec == i);
  }

  // Test compute_esphome_weekday across Sunday, Saturday, Monday, leap day, and invalid inputs.
  assert(brd_rtc::compute_esphome_weekday(2026, 11, 15) == 1);  // Sunday
  assert(brd_rtc::compute_esphome_weekday(2026, 11, 16) == 2);  // Monday
  assert(brd_rtc::compute_esphome_weekday(2026, 11, 14) == 7);  // Saturday
  assert(brd_rtc::compute_esphome_weekday(2024, 2, 29) == 5);   // Leap Thursday
  assert(brd_rtc::compute_esphome_weekday(2026, 0, 15) == 0);
  assert(brd_rtc::compute_esphome_weekday(2026, 13, 15) == 0);
  assert(brd_rtc::compute_esphome_weekday(2026, 11, 0) == 0);

  MockI2cBus mock;

  // Test 3-attempt bounded retry success.
  mock.fail_count = 2;
  const uint8_t test_data[2] = {0x01, 0x00};
  assert(brd_rtc::retry_write(mock, 0x51, test_data, 2));

  // Test 3-attempt bounded retry exhaustion.
  mock.fail_count = 3;
  assert(!brd_rtc::retry_write(mock, 0x51, test_data, 2));

  // Test read_and_clear_status_and_disable_clkout.
  mock.fail_count = 0;
  mock.registers[0x01] = 0x0C;
  mock.registers[0x0D] = 0x80;
  brd_rtc::RtcStatus status;
  assert(brd_rtc::read_and_clear_status_and_disable_clkout(mock, 0x51, status));
  assert(status.alarm_flag && status.timer_flag);
  assert(mock.registers[0x01] == 0x00);
  assert(mock.registers[0x0D] == 0x00);

  // Test write_utc_clock_only writes ESPTime weekday 1..7 (2026-11-16 is Monday -> 2).
  brd_rtc::UtcClock clk;
  clk.year = 2026;
  clk.month = 11;
  clk.day = 16;
  clk.weekday = 0;
  clk.hour = 12;
  clk.minute = 34;
  clk.second = 56;
  assert(brd_rtc::write_utc_clock_only(mock, 0x51, clk));
  assert(mock.registers[0x02] == brd_rtc::dec_to_bcd(56));
  assert(mock.registers[0x03] == brd_rtc::dec_to_bcd(34));
  assert(mock.registers[0x04] == brd_rtc::dec_to_bcd(12));
  assert(mock.registers[0x05] == brd_rtc::dec_to_bcd(16));
  assert(mock.registers[0x06] == brd_rtc::dec_to_bcd(2));
  assert(mock.registers[0x07] == brd_rtc::dec_to_bcd(11));
  assert(mock.registers[0x08] == brd_rtc::dec_to_bcd(26));
  assert(mock.registers[0x0D] == 0x00);

  // Test write_utc_clock_only writes 1 for Sunday, clears VL bit, and clears STOP bit.
  mock.registers[0x00] = 0x20;  // STOP=1
  clk.year = 2026;
  clk.month = 11;
  clk.day = 15;
  clk.weekday = 0;
  assert(brd_rtc::write_utc_clock_only(mock, 0x51, clk));
  assert(mock.registers[0x00] == 0x00);
  assert(mock.registers[0x06] == 1);
  assert((mock.registers[0x02] & 0x80) == 0);

  // Test write_utc_clock_only rejects out-of-range fields.
  brd_rtc::UtcClock bad_clk = clk;
  bad_clk.year = 1999;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));
  bad_clk = clk;
  bad_clk.hour = 24;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));
  bad_clk = clk;
  bad_clk.minute = 60;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));
  bad_clk = clk;
  bad_clk.second = 60;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));

  // Test repair_sunday_weekday_if_valid repairs 0 -> 1 when VL == 0 on Sunday 2026-11-15.
  mock.registers[0x02] = brd_rtc::dec_to_bcd(10);  // VL == 0
  mock.registers[0x05] = brd_rtc::dec_to_bcd(15);
  mock.registers[0x06] = 0;  // Midnight rollover from Saturday 7 -> 0
  mock.registers[0x07] = brd_rtc::dec_to_bcd(11);
  mock.registers[0x08] = brd_rtc::dec_to_bcd(26);
  assert(brd_rtc::repair_sunday_weekday_if_valid(mock, 0x51));
  assert(mock.registers[0x06] == 1);

  // Test repair_sunday_weekday_if_valid leaves register untouched when VL == 1.
  mock.registers[0x02] = static_cast<uint8_t>(0x80 | brd_rtc::dec_to_bcd(10));  // VL == 1
  mock.registers[0x06] = 0;
  assert(brd_rtc::repair_sunday_weekday_if_valid(mock, 0x51));
  assert(mock.registers[0x06] == 0);

  // Test arm_wake in alarm mode.
  brd_rtc::WakeSettings ws_alarm;
  ws_alarm.use_alarm = true;
  ws_alarm.target_utc_hour = 5;
  ws_alarm.target_utc_minute = 20;
  ws_alarm.sleep_seconds = 28800;
  assert(brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert((mock.registers[0x01] & 0x02) != 0);
  assert(mock.registers[0x09] == brd_rtc::dec_to_bcd(20));
  assert(mock.registers[0x0A] == brd_rtc::dec_to_bcd(5));
  assert(mock.registers[0x0B] == 0x80);
  assert(mock.registers[0x0C] == 0x80);

  // Test arm_wake readback mismatch returning false.
  mock.inject_readback_mismatch = true;
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  mock.inject_readback_mismatch = false;

  // Test arm_wake in timer mode (<= 255s -> 1 Hz mode 0x82).
  brd_rtc::WakeSettings ws_timer_sec;
  ws_timer_sec.use_alarm = false;
  ws_timer_sec.sleep_seconds = 120;
  assert(brd_rtc::arm_wake(mock, 0x51, ws_timer_sec));
  assert((mock.registers[0x01] & 0x01) != 0);
  assert(mock.registers[0x0E] == 0x82);
  assert(mock.registers[0x0F] == 120);

  // Test arm_wake in timer mode (> 255s -> 1/60 Hz mode 0x83).
  // Sleep duration of 898 seconds rounds to 15 minutes: (898 + 30) / 60 = 15.
  brd_rtc::WakeSettings ws_timer_min;
  ws_timer_min.use_alarm = false;
  ws_timer_min.sleep_seconds = 898;
  assert(brd_rtc::arm_wake(mock, 0x51, ws_timer_min));
  assert((mock.registers[0x01] & 0x01) != 0);
  assert(mock.registers[0x0E] == 0x83);
  assert(mock.registers[0x0F] == 15);

  printf("  -> Passed BCD and RTC driver checks.\n");
}

static void test_power_state_and_telemetry() {
  printf("[TEST] brd_power state CRC, classification, USB hysteresis, and telemetry...\n");

  // Test voltage_to_mv conversion and boundary guards.
  assert(brd_power::voltage_to_mv(std::numeric_limits<float>::quiet_NaN()) == 0U);
  assert(brd_power::voltage_to_mv(-1.0f) == 0U);
  assert(brd_power::voltage_to_mv(0.0f) == 0U);
  assert(brd_power::voltage_to_mv(4.196f) == 4196U);
  assert(brd_power::voltage_to_mv(15.0f) == 0U);

  // Shift register bit constants.
  assert(brd_power::CFG_RAILS_ONLY_ON == 0x22);
  assert(brd_power::CFG_ALL_OFF_SMPS_DISABLED == 0x02);

  // Table-driven tests for battery_voltage_to_percent.
  assert(brd_power::battery_voltage_to_percent(std::numeric_limits<float>::quiet_NaN()) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(std::numeric_limits<float>::infinity()) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(-std::numeric_limits<float>::infinity()) == 0.0f);

  // Clamping below minimum discharge threshold.
  assert(brd_power::battery_voltage_to_percent(-1.00f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(0.00f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(2.50f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(2.99f) == 0.0f);

  // Clamping above full charge threshold.
  assert(brd_power::battery_voltage_to_percent(4.20f) == 100.0f);
  assert(brd_power::battery_voltage_to_percent(4.21f) == 100.0f);
  assert(brd_power::battery_voltage_to_percent(4.50f) == 100.0f);
  assert(brd_power::battery_voltage_to_percent(5.00f) == 100.0f);

  // Exact curve points matching upstream battery driver.
  struct BatteryCurvePoint {
    float voltage;
    float expected_percent;
  };
  const BatteryCurvePoint curve_points[] = {
      {4.20f, 100.0f},
      {4.10f, 92.0f},
      {4.00f, 83.0f},
      {3.90f, 72.0f},
      {3.80f, 57.0f},
      {3.70f, 41.0f},
      {3.60f, 24.0f},
      {3.50f, 13.0f},
      {3.30f, 4.0f},
      {3.00f, 0.0f},
  };
  for (const auto &pt : curve_points) {
    float actual = brd_power::battery_voltage_to_percent(pt.voltage);
    assert(std::fabs(actual - pt.expected_percent) < 0.01f);
  }

  // Linear interpolation midpoints.
  assert(std::fabs(brd_power::battery_voltage_to_percent(4.15f) - 96.0f) < 0.01f);
  assert(std::fabs(brd_power::battery_voltage_to_percent(3.15f) - 2.0f) < 0.01f);

  // Monotonicity verification from 3.00 V to 4.20 V.
  float prev_pct = -1.0f;
  for (float v = 3.00f; v <= 4.201f; v += 0.005f) {
    float pct = brd_power::battery_voltage_to_percent(v);
    assert(pct >= prev_pct);
    assert(pct >= 0.0f && pct <= 100.0f);
    prev_pct = pct;
  }

  // Touch point coordinate bounds verification (landscape 960x540 and portrait 540x960).
  assert(brd_power::is_valid_touch_point(0, 0, 960, 540));
  assert(brd_power::is_valid_touch_point(959, 539, 960, 540));
  assert(brd_power::is_valid_touch_point(480, 270, 960, 540));
  assert(!brd_power::is_valid_touch_point(-1, 0, 960, 540));
  assert(!brd_power::is_valid_touch_point(0, -1, 960, 540));
  assert(!brd_power::is_valid_touch_point(960, 0, 960, 540));
  assert(!brd_power::is_valid_touch_point(0, 540, 960, 540));
  assert(!brd_power::is_valid_touch_point(960, 540, 960, 540));
  assert(!brd_power::is_valid_touch_point(1000, 200, 960, 540));
  assert(!brd_power::is_valid_touch_point(500, 600, 960, 540));
  assert(!brd_power::is_valid_touch_point(-100, -100, 960, 540));

  // Portrait 540x960 bounds and invalid display dimension checks.
  assert(brd_power::is_valid_touch_point(200, 750, 540, 960));
  assert(brd_power::is_valid_touch_point(539, 959, 540, 960));
  assert(!brd_power::is_valid_touch_point(540, 500, 540, 960));
  assert(!brd_power::is_valid_touch_point(200, 960, 540, 960));
  assert(!brd_power::is_valid_touch_point(10, 10, 0, 960));
  assert(!brd_power::is_valid_touch_point(10, 10, 540, 0));
  assert(!brd_power::is_valid_touch_point(10, 10, -540, 960));
  assert(!brd_power::is_valid_touch_point(10, 10, 540, -960));

  // RtcPersistentState CRC validation and reset.
  brd_power::RtcPersistentState state;
  state.reset();
  assert(state.validate_or_reset());
  assert(state.boot_count == 0);
  assert(state.magic == brd_power::RTC_STATE_MAGIC);

  state.boot_count = 42;
  state.prev_batt_mv = 4120;
  state.staying_awake_usb = true;
  state.prev_gt911 = 2;
  state.last_gt911_addr = 2;
  state.update_crc();
  assert(state.validate_or_reset());
  assert(state.boot_count == 42);
  assert(state.last_gt911_addr == 2);

  // Clearing telemetry prev_gt911 preserves cached last_gt911_addr.
  state.prev_gt911 = -1;
  state.update_crc();
  assert(state.validate_or_reset());
  assert(state.prev_gt911 == -1);
  assert(state.last_gt911_addr == 2);

  // Corrupt state single-bit triggers auto-reset.
  state.boot_count ^= 0x01;
  assert(!state.validate_or_reset());
  assert(state.boot_count == 0);
  assert(state.last_gt911_addr == 0);
  assert(state.validate_or_reset());

  // Sleep lock reference counting and sleep eligibility predicate.
  brd_power::sleep_lock_count = 0;
  assert(!brd_power::is_sleep_locked());
  brd_power::release_sleep_lock();  // Underflow stays clamped at 0.
  assert(!brd_power::is_sleep_locked());
  brd_power::acquire_sleep_lock();
  brd_power::acquire_sleep_lock();
  assert(brd_power::is_sleep_locked() && brd_power::sleep_lock_count == 2U);
  brd_power::release_sleep_lock();
  assert(brd_power::is_sleep_locked() && brd_power::sleep_lock_count == 1U);
  brd_power::release_sleep_lock();
  assert(!brd_power::is_sleep_locked() && brd_power::sleep_lock_count == 0U);

  assert(brd_power::can_enter_sleep(false, false, false, 0U));
  assert(!brd_power::can_enter_sleep(true, false, false, 0U));
  assert(!brd_power::can_enter_sleep(false, true, false, 0U));
  assert(!brd_power::can_enter_sleep(false, false, true, 0U));
  assert(!brd_power::can_enter_sleep(false, false, false, 1U));

  // Wake classification table tests.
  assert(std::strcmp(brd_power::classify_wake_reason(3, (1ULL << 21), false, false), "Ext_BTN") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(3, (1ULL << 9), false, false), "Ext_RTC") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(3, (1ULL << 47), false, false), "Ext_Touch") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(3, (1ULL << 10), false, false), "Ext_Touch") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(3, 0, true, false), "Ext_RTC") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(3, 0, false, true), "Ext_RTC") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(4, 0, false, false), "Int_RTC") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(0, 0, false, false), "Power_On") == 0);

  // USB power evaluation tests across all 5 conditions:
  // 1. SOF increment returns true unconditionally.
  assert(brd_power::evaluate_usb_power(0, 0, false, true, false));
  assert(brd_power::evaluate_usb_power(3500, 3500, false, true, true));

  // 2. Currently awake returns true when voltage >= 4185 mV.
  assert(brd_power::evaluate_usb_power(4185, 4200, true, false, false));
  assert(!brd_power::evaluate_usb_power(4184, 4200, true, false, false));

  // 3. Voltage rise >= 25 mV returns true.
  assert(brd_power::evaluate_usb_power(4175, 4150, false, false, false));
  assert(!brd_power::evaluate_usb_power(4174, 4150, false, false, false));

  // 4. Cold boot with voltage >= 4195 mV returns true.
  assert(brd_power::evaluate_usb_power(4195, 0, false, false, false));
  assert(!brd_power::evaluate_usb_power(4194, 0, false, false, false));
  // Not a cold boot when prev_slept is true.
  assert(!brd_power::evaluate_usb_power(4195, 0, false, false, true));

  // 5. None of the above returns false.
  assert(!brd_power::evaluate_usb_power(4100, 4100, false, false, false));

  // Table-driven tests for RTC/NTP sync helpers.
  struct SyncDueCase {
    bool rtc_valid;
    uint32_t current_epoch;
    uint32_t last_sync_epoch;
    bool expected_due;
  };
  constexpr SyncDueCase SYNC_DUE_CASES[] = {
      {false, 1795500000U, 1795499900U, true},   // Invalid RTC clock
      {true, 1795500000U, 0U, true},             // Never synced
      {true, 1795499999U, 1795500000U, true},    // Clock rolled back
      {true, 1795500000U, 1795500000U, false},   // Just synced
      {true, 1795521599U, 1795500000U, false},   // 1 second before 6h interval
      {true, 1795521600U, 1795500000U, true},    // Exact 6h boundary
      {true, 1795530000U, 1795500000U, true},    // Past 6h interval
  };
  for (const auto &tc : SYNC_DUE_CASES) {
    assert(brd_power::is_rtc_sync_due(tc.rtc_valid, tc.current_epoch, tc.last_sync_epoch) ==
           tc.expected_due);
  }

  struct SyncStatusCase {
    bool requested;
    bool ntp_synced;
    bool rtc_saved;
    const char *expected_status;
    int expected_metric;
  };
  constexpr SyncStatusCase SYNC_STATUS_CASES[] = {
      {false, false, false, "skipped", 0},
      {false, false, true, "skipped", 0},
      {true, false, false, "timeout", -1},
      {true, false, true, "timeout", -1},
      {false, true, false, "save_err", -1},
      {true, true, false, "save_err", -1},
      {false, true, true, "synced", 1},
      {true, true, true, "synced", 1},
  };
  for (const auto &tc : SYNC_STATUS_CASES) {
    assert(std::strcmp(brd_power::classify_rtc_sync_status(tc.requested, tc.ntp_synced, tc.rtc_saved),
                       tc.expected_status) == 0);
    assert(brd_power::classify_rtc_sync_metric(tc.requested, tc.ntp_synced, tc.rtc_saved) ==
           tc.expected_metric);
  }

  assert(brd_power::compute_rtc_sync_age(0U, 1795500000U) == -1);
  assert(brd_power::compute_rtc_sync_age(1795500000U, 0U) == -1);
  assert(brd_power::compute_rtc_sync_age(1795499999U, 1795500000U) == -1);
  assert(brd_power::compute_rtc_sync_age(1795500000U, 1795500000U) == 0);
  assert(brd_power::compute_rtc_sync_age(1795500900U, 1795500000U) == 900);

  // Telemetry formatters.
  brd_power::TraceTelemetry telem;
  telem.wake_reason = "Ext_RTC";
  telem.mode = "timer";
  telem.batt_v = 4.12f;
  telem.batt_pct = 92.0f;
  telem.awake_s = 2.4f;
  telem.image_status = "new";
  telem.touch_x = -1;
  telem.touch_y = -1;
  telem.now_str = "2026-11-16T01:30:00+0100";
  telem.next_str = "2026-11-16T01:45:00+0100";
  telem.next_in_str = "900";
  telem.since_str = "900";
  telem.prev_slept = true;
  telem.prev_gt911 = 1;
  telem.boot_count = 15;
  telem.job_name = "brd";
  telem.since_num = 900;
  telem.rtc_sync = "synced";
  telem.rtc_sync_code = 1;
  telem.rtc_sync_age_sec = 0;

  char vlogs_msg[384];
  int vlen = brd_power::format_vlogs_message(vlogs_msg, sizeof(vlogs_msg), telem);
  assert(vlen > 0 && static_cast<size_t>(vlen) < sizeof(vlogs_msg));
  assert(std::strstr(vlogs_msg, "wake=Ext_RTC mode=timer batt=4.12V batt_pct=92% awake=2.4s image=new touch=none") != nullptr);
  assert(std::strstr(vlogs_msg, "prev_gt911=0x5D boots=15 rtc_sync=synced rtc_sync_age=0s") != nullptr);

  char vmetrics_prom[448];
  int plen = brd_power::format_vmetrics_prometheus(vmetrics_prom, sizeof(vmetrics_prom), telem);
  assert(plen > 0 && static_cast<size_t>(plen) < sizeof(vmetrics_prom));
  assert(std::strstr(vmetrics_prom, "battery_voltage{job=\"brd\"} 4.12") != nullptr);
  assert(std::strstr(vmetrics_prom, "battery_percent{job=\"brd\"} 92") != nullptr);
  assert(std::strstr(vmetrics_prom, "gt911_slept{job=\"brd\"} 1") != nullptr);
  assert(std::strstr(vmetrics_prom, "boot_count{job=\"brd\"} 15") != nullptr);
  assert(std::strstr(vmetrics_prom, "rtc_synced{job=\"brd\"} 1") != nullptr);
  assert(std::strstr(vmetrics_prom, "rtc_sync_age_secs{job=\"brd\"} 0") != nullptr);

  // Telemetry formatting when RTC sync timed out and age is unknown.
  telem.rtc_sync = "timeout";
  telem.rtc_sync_code = -1;
  telem.rtc_sync_age_sec = -1;
  vlen = brd_power::format_vlogs_message(vlogs_msg, sizeof(vlogs_msg), telem);
  assert(vlen > 0 && static_cast<size_t>(vlen) < sizeof(vlogs_msg));
  assert(std::strstr(vlogs_msg, "rtc_sync=timeout rtc_sync_age=unknown") != nullptr);
  plen = brd_power::format_vmetrics_prometheus(vmetrics_prom, sizeof(vmetrics_prom), telem);
  assert(plen > 0 && static_cast<size_t>(plen) < sizeof(vmetrics_prom));
  assert(std::strstr(vmetrics_prom, "rtc_synced{job=\"brd\"} -1") != nullptr);
  assert(std::strstr(vmetrics_prom, "rtc_sync_age_secs{job=\"brd\"} -1") != nullptr);

  printf("  -> Passed power state and telemetry checks.\n");
}

static void test_multithreaded_concurrency() {
  printf("[TEST] Multithreaded concurrency test (6 threads, 1000 iterations)...\n");

  brd_blit::BlitBuffer shared_buffer;
  constexpr int ITERATIONS = 1000;

  std::thread t1([&shared_buffer]() {
    std::vector<uint8_t> data(brd_blit::FRAMEBUFFER_SIZE);
    for (int i = 0; i < ITERATIONS; ++i) {
      std::memset(data.data(), static_cast<uint8_t>(i & 0xFF), data.size());
      shared_buffer.store(data.data(), data.size());
    }
  });

  std::thread t2([&shared_buffer]() {
    std::vector<uint8_t> data(brd_blit::FRAMEBUFFER_SIZE);
    for (int i = 0; i < ITERATIONS; ++i) {
      shared_buffer.retrieve(data.data(), data.size());
    }
  });

  std::thread t3([]() {
    brd_schedule::PosixTzCalendar cal("CET-1CEST,M3.5.0,M10.5.0/3");
    brd_schedule::ScheduleConfig cfg;
    for (int i = 0; i < ITERATIONS; ++i) {
      int64_t epoch = 1782000000 + i * 60;
      auto plan = brd_schedule::plan_next_wake(cal, epoch, cfg);
      assert(plan.clock_valid);
    }
  });

  std::thread t4([]() {
    brd_power::RtcPersistentState local_state;
    local_state.reset();
    for (int i = 0; i < ITERATIONS; ++i) {
      local_state.boot_count = static_cast<uint32_t>(i);
      local_state.update_crc();
      assert(local_state.validate_or_reset());
    }
  });

  std::thread t5([]() {
    brd_schedule::PosixTzCalendar cal_ny("EST5EDT,M3.2.0,M11.1.0");
    for (int i = 0; i < ITERATIONS; ++i) {
      int64_t epoch = 1782000000 + i * 60;
      brd_schedule::WallTime wt;
      assert(cal_ny.epoch_to_local(epoch, wt));
      int64_t back_epoch = 0;
      assert(cal_ny.local_to_epoch(wt, back_epoch));
      assert(back_epoch == epoch);
    }
  });

  std::thread t6([]() {
    brd_schedule::PosixTzCalendar cal_utc("UTC");
    for (int i = 0; i < ITERATIONS; ++i) {
      int64_t epoch = 1782000000 + i * 60;
      brd_schedule::WallTime wt;
      assert(cal_utc.epoch_to_local(epoch, wt));
      int64_t back_epoch = 0;
      assert(cal_utc.local_to_epoch(wt, back_epoch));
      assert(back_epoch == epoch);
    }
  });

  t1.join();
  t2.join();
  t3.join();
  t4.join();
  t5.join();
  t6.join();

  printf("  -> Passed multithreaded concurrency test cleanly.\n");
}

int main() {
  printf("=========================================\n");
  printf("Starting brd test suite (C++20)\n");
  printf("=========================================\n");

  test_blit_bounds_and_errors();
  test_blit_sha256_normalizer();
  test_blit_thread_safe_buffer();
  test_schedule_civil_normalization();
  test_schedule_dst_and_boundaries();
  test_rtc_bcd_and_retries();
  test_power_state_and_telemetry();
  test_multithreaded_concurrency();

  printf("=========================================\n");
  printf("ALL BRD TESTS PASSED (0 ERRORS)\n");
  printf("=========================================\n");
  return 0;
}
