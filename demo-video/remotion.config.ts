import { Config } from '@remotion/cli/config';

// JPEG frames render faster than PNG and the output is H.264 anyway; the screen captures are JPEG themselves.
Config.setVideoImageFormat('jpeg');
Config.setJpegQuality(95);
Config.setOverwriteOutput(true);
// Every frame shows a different capture, so a few browser tabs in parallel keep the render moving.
Config.setConcurrency(4);
