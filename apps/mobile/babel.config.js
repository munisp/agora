// babel-preset-expo is the standard Expo SDK 51 preset; it already includes
// the expo-router transform, so no extra plugins are required here.
module.exports = function (api) {
  api.cache(true);
  return {
    presets: ["babel-preset-expo"],
  };
};
