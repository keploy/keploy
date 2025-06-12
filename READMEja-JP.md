<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ ユーザートラフィックからのユニットテストよりも速いAPIテスト ⚡️
</b>
</h3 >
<p align="center">
🌟 AI-Gen時代の開発者に必須のツール 🌟
</p>

---

<h4 align="center">

   <a href="https://x.com/Keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X" />
  </a>

<a href="https://github.com/Keploy/Keploy/">
   <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help%20us%20reach%2020K%20stars!%20Now%20at:" alt="Help us reach 20k stars!" />
  </a>

  <a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Keploy CNCF Landscape" />
  </a>

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

</h4>


[Keploy](https://keploy.io) は、**開発者中心**のAPIテストツールで、**組み込みモック**を使用してユニットテストよりも速くテストを作成します。

KeployはAPI呼び出しだけでなく、データベース呼び出しも記録し、テスト中に再生するため、**使いやすく、強力で、拡張性があります**。

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>

> 🐰 **面白い事実:** Keployは自分自身をテストに使用しています！私たちの素晴らしいカバレッジバッジをチェックしてください: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 [ユニットテストジェネレーター](README-UnitGen.md) (ut-gen) のためにここにいますか？
Keployは、[Meta LLM研究論文](https://arxiv.org/pdf/2402.09171)の世界初のユニットテストジェネレーター(ut-gen)実装を新たに発表しました。これはコードのセマンティクスを理解し、意味のあるユニットテストを生成します。目指すのは：

- **ユニットテスト生成の自動化 (UTG)**: 包括的なユニットテストを迅速に生成し、冗長な手動作業を削減します。

- **エッジケースの改善**: 自動テストの範囲を拡張し、手動で見逃されがちな複雑なシナリオをカバーします。

- **テストカバレッジの向上**: コードベースが成長するにつれて、徹底的なカバレッジを確保することが可能になります。

### 📜 [ユニットテストジェネレーター README](README-UnitGen.md) をフォローしてください！ ✅

## 📘 ドキュメント！
**[Keploy Documentation](https://keploy.io/docs/)** でKeployのプロフェッショナルになりましょう。

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>

# 🚀 クイックインストール (APIテストジェネレーター)

エージェントをローカルにインストールしてKeployを統合します。コード変更は不要です。

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

##  🎬 テストケースの記録

API呼び出しをテストとモック/スタブに変換するために、Keployを使用してアプリを開始します。

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```
例えば、シンプルなPythonアプリを使用している場合、`CMD_TO_RUN_APP`は`python main.py`、Golangの場合は`go run main.go`、Javaの場合は`java -jar xyz.jar`、Nodeの場合は`npm start`のようになります。

```zsh
keploy record -c "python main.py"
```

## 🧪 テストの実行
データベース、Redis、Kafka、またはアプリケーションが使用する他のサービスをシャットダウンします。Keployはテスト中にそれらを必要としません。
```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ テストカバレッジの統合
ユニットテストライブラリと統合して、結合テストカバレッジを表示するには、この[テストカバレッジガイド](https://keploy.io/docs/server/sdk-installation/go/)に従ってください。

> ####  **楽しんでいただけましたか:** このリポジトリに🌟スターを残してください！無料で笑顔をもたらします。😄 👏

## ワンクリックセットアップ 🚀

ローカルマシンのインストールなしでKeployを迅速にセットアップして実行します：

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 質問がありますか？
私たちに連絡してください。お手伝いします！

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)


## 🌐 言語サポート
Goのゴーファー 🐹 からPythonのスネーク 🐍 まで、以下の言語をサポートしています：

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Keployの採用者 🧡

あなたとあなたの組織がKeployを使用しているのですか？それは素晴らしいことです。 [**このリスト**](https://github.com/orgs/keploy/discussions/1765) に追加してください。グッズをお送りします！💖

私たちは、あなたたち全員が私たちのコミュニティの一員であることを誇りに思います！💖

## 🎩 魔法はどのように起こるのか？
Keployプロキシは、アプリの**すべての**ネットワークインタラクション（CRUD操作、非冪等なAPIを含む）をキャプチャして再生します。

**[Keployの仕組み](https://keploy.io/docs/keploy-explained/how-keploy-works/)** の旅に出て、カーテンの裏にあるトリックを発見してください！

ここにKeployの主な機能があります: 🛠

- ♻️ **結合テストカバレッジ:** Keployテストをお気に入りのテストライブラリ（JUnit、go-test、py-test、jest）と統合して、結合テストカバレッジを表示します。

- 🤖 **EBPFインストルメンテーション:** KeployはEBPFを使用して、コードレス、言語非依存、非常に軽量な統合を実現します。

- 🌐 **CI/CD統合:** テストをローカルCLI、CIパイプライン（Jenkins、Github Actions..）、またはKubernetesクラスター全体で実行します。

- 📽️ **複雑なフローの記録と再生:** Keployは、複雑で分散したAPIフローをモックとスタブとして記録して再生できます。これは、テストのためのタイムマシンを持っているようなもので、たくさんの時間を節約できます！

- 🎭 **多目的モック:** Keployモックをサーバーテストとしても使用できます！

## 👨🏻‍💻 一緒に構築しましょう！ 👩🏻‍💻
初心者のコーダーでもウィザードでも 🧙‍♀️、あなたの視点は貴重です。以下をチェックしてください：

📜 [貢献ガイドライン](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [行動規範](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)


## 🐲 現在の制限事項！
- **ユニットテスト:** Keployはユニットテストフレームワーク（Go test、JUnit..）と一緒に実行するように設計されており、全体的なコードカバレッジに追加することができますが、それでも統合テストを生成します。
- **プロダクション環境:** Keployは現在、開発者向けのテスト生成に焦点を当てています。これらのテストは任意の環境からキャプチャできますが、高ボリュームのプロダクション環境ではテストしていません。これは、過剰な冗長テストのキャプチャを避けるために堅牢な重複排除が必要です。堅牢な重複排除システムの構築についてのアイデアがあります [#27](https://github.com/keploy/keploy/issues/27)

## ✨ リソース！
🤔 [FAQ](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [なぜKeploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [インストールガイド](https://keploy.io/docs/application-development/)

📖 [貢献ガイド](https://keploy.io/docs/keploy-explained/contribution-guide/)
