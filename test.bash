#!/bin/bash
# Test the torrent

./taipeitorrent -torrent=testData/a.torrent -fileDir=testData/downloads -port 63881 -useUPnP -useDHT
